package graphql

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
)

// InputFieldOption declares one field of an Input or Args binding.
type InputFieldOption interface {
	applyInput(*inputBinding)
}

// inputBinding collects setters for a Go struct T that receives either an
// input object (name set) or a field's arguments (name empty).
type inputBinding struct {
	name   string
	goType reflect.Type
	fields []*inputFieldSpec
	dec    *inputDecoder // shared decoder for Input; nil for Args
	// zeroForNull lets a Go type that cannot be null back a nullable input
	// position, decoding absent and null alike to the zero value.
	zeroForNull bool
	// derived is set when every field was derived from the struct, so each
	// setter writes its own struct field and the order they run in cannot
	// matter. A hand-written InputField is arbitrary code, and may.
	derived bool
}

// inputFlag is an Input option that configures the binding rather than
// declaring a field, so it must not suppress field derivation.
type inputFlag struct{ zeroForNull bool }

func (f inputFlag) applyInput(ib *inputBinding) {
	if f.zeroForNull {
		ib.zeroForNull = true
	}
}

// inputFieldSpec is an input field binding before it is resolved against the
// SDL definition of the field or argument.
type inputFieldSpec struct {
	name      string
	valueType reflect.Type
	// targetType is the struct a hand-written setter writes into, nil for a
	// derived field. The setter is handed the binding's own struct and
	// asserts it to this type, so the two have to be the same one.
	targetType reflect.Type
	resolve    func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error)
}

func (f *inputFieldSpec) applyInput(ib *inputBinding) { ib.fields = append(ib.fields, f) }

// inputDecoder decodes a raw value tree into *T. For Args registrations the
// decoder is a template carrying argsBinding, specialised per field by build.
type inputDecoder struct {
	name        string
	goType      reflect.Type
	newValue    func() any
	setters     []*inputSetter
	argsBinding *inputBinding
	// byName and absent exist when the setters are order-independent (see
	// inputBinding.derived): decode then visits the fields the value has, and
	// of the rest only those an absent value means something for -- a
	// default to apply or a required field to refuse. An ent-style WhereInput
	// declares a hundred fields and a request sends two.
	byName map[string]*inputSetter
	absent []*inputSetter
	// copier makes a request its own copy of arguments decoded from literals,
	// in place of decoding them again; see argsCopier. A setter written by
	// hand has one too: it may put what it is handed in two fields, or deeper
	// in the struct, and sharing looks at the value it produced for exactly
	// that before a copy is trusted with it. It depends on the Go type alone,
	// so the template an Args binding registers builds it once, copierBuilt,
	// for every field that takes those arguments.
	copier      *argsCopier
	copierBuilt bool
}

// inputSetter decodes one input field or argument into its struct field.
type inputSetter struct {
	name       string
	typ        *ast.Type
	hasDefault bool
	defaultRaw any
	set        func(target, raw any) error
}

func newSetter(name string, typ *ast.Type, def *ast.Value, set func(target, raw any) error) (*inputSetter, error) {
	st := &inputSetter{name: name, typ: typ, set: set}
	if def == nil {
		return st, nil
	}
	raw, err := astJSON(def, nil)
	if err != nil {
		return nil, fmt.Errorf("field %q: invalid default value: %w", name, err)
	}
	st.hasDefault = true
	st.defaultRaw = raw
	return st, nil
}

// checkDefault decodes the setter's default into target, which the caller
// discards, the way decodeOrdered does for a request that leaves the field
// out. gqlparser holds a default to SDL syntax and not to the field's type, so
// `n: Int = "abc"` loaded, and every request relying on the default failed
// over a value its client never sent. The error names the default as written.
func (st *inputSetter) checkDefault(target any, def *ast.Value) error {
	if !st.hasDefault {
		return nil
	}
	err := errNonNull
	if st.defaultRaw != nil || !st.typ.NonNull {
		err = st.set(target, st.defaultRaw)
	}
	if err != nil {
		return fmt.Errorf("its default %s is not a value it accepts: %w", def.String(), err)
	}
	return nil
}

// decode applies every setter to a freshly allocated *T. Absent fields fall
// back to their SDL default; absent nullable fields without a default are
// left at their zero value and, for OmittableField, remain unset.
//
// With byName it visits the value's own keys instead of every declared field.
// A map has no order, so on any failure it decodes again in declared order,
// which is what reports the error: the same one, every time, as before.
func (d *inputDecoder) decode(m map[string]any) (any, error) {
	if d.byName != nil {
		if v, ok := d.decodePresent(m); ok {
			return v, nil
		}
	}
	return d.decodeOrdered(m)
}

func (d *inputDecoder) decodePresent(m map[string]any) (any, bool) {
	target := d.newValue()
	for name, raw := range m {
		st := d.byName[name]
		if st == nil {
			continue
		}
		if raw == nil && st.typ.NonNull {
			return nil, false
		}
		if st.set(target, raw) != nil {
			return nil, false
		}
	}
	for _, st := range d.absent {
		if _, present := m[st.name]; present {
			continue
		}
		if !st.hasDefault || (st.defaultRaw == nil && st.typ.NonNull) {
			return nil, false
		}
		if st.set(target, st.defaultRaw) != nil {
			return nil, false
		}
	}
	return target, true
}

func (d *inputDecoder) decodeOrdered(m map[string]any) (any, error) {
	target := d.newValue()
	for _, st := range d.setters {
		raw, present := m[st.name]
		if !present {
			if !st.hasDefault {
				if st.typ.NonNull {
					return nil, fmt.Errorf("field %q of required type %s was not provided", st.name, st.typ.String())
				}
				continue
			}
			raw = st.defaultRaw
		}
		if raw == nil && st.typ.NonNull {
			return nil, fmt.Errorf("field %q of non-null type %s must not be null", st.name, st.typ.String())
		}
		if err := st.set(target, raw); err != nil {
			return nil, fmt.Errorf("field %q: %w", st.name, err)
		}
	}
	return target, nil
}

// Input binds the GraphQL input object type name to the Go struct T.
func Input[T any](name string, fields ...InputFieldOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		if dec := bindInput(b, name, reflect.TypeFor[T](), func() any { return new(T) }, fields); dec != nil {
			registerInputShapes(b.reg, name, reflect.TypeFor[T](), dec)
		}
	})
}

// bindInput is Input without its type parameter; it returns nil when the
// binding is refused.
func bindInput(b *schemaBuilder, name string, tT reflect.Type, newValue func() any, fields []InputFieldOption) *inputDecoder {
	def := b.ast.Types[name]
	if def == nil || def.Kind != ast.InputObject {
		b.errorf("Input %q: type is not an input object in the schema", name)
		return nil
	}
	if tT.Kind() != reflect.Struct {
		b.errorf("Input %q: Go type %s must be a struct", name, tT)
		return nil
	}
	if prev := b.reg.inputsByName[name]; prev != nil {
		b.errorf("Input %q: already bound to %s", name, prev.goType)
		return nil
	}
	dec := &inputDecoder{name: name, goType: tT, newValue: newValue}
	ib := &inputBinding{name: name, goType: tT, dec: dec}
	// Options first: ZeroForNull declares nothing, so deriving on
	// "no options given" would make it suppress the derivation it is
	// meant to configure.
	for _, f := range fields {
		f.applyInput(ib)
	}
	if len(ib.fields) == 0 {
		for _, f := range autoInputFields(tT, ib.zeroForNull, declaredInputFields(def)) {
			f.applyInput(ib)
		}
		ib.derived = true
	}
	b.inputs = append(b.inputs, ib)
	b.reg.inputsByName[name] = dec
	return dec
}

// declaredInputFields is the set of field names the schema declares on def.
func declaredInputFields(def *ast.Definition) map[string]bool {
	m := make(map[string]bool, len(def.Fields))
	for _, f := range def.Fields {
		m[f.Name] = true
	}
	return m
}

// registerInputShapes registers decoders for T, *T, []T and []*T that all
// route through dec, which is completed later by inputBinding.resolve.
//
// They are type-erased and reflect on the slices: decoding input is where
// reflection is allowed, and a generic decoder per shape was compiled again
// in every package binding an input -- once per input type, in generated
// code that binds hundreds. A hand-written InputField of one of these shapes
// gets a typed decoder from inputDecoderFor, in the package that asked.
func registerInputShapes(r *registry, name string, tT reflect.Type, dec *inputDecoder) {
	tPT := reflect.PointerTo(tT)
	tST := reflect.SliceOf(tT)
	tSPT := reflect.SliceOf(tPT)
	// A null decodes to a typed nil, which the setter can store.
	nilPT, nilST, nilSPT := reflect.Zero(tPT).Interface(), reflect.Zero(tST).Interface(), reflect.Zero(tSPT).Interface()

	r.addInputShape(name, []bool{false}, tT, func(raw any, _ *ast.Type) (any, error) {
		if raw == nil {
			return nil, errNonNull
		}
		p, err := dec.decodeRaw(raw)
		if err != nil {
			return nil, err
		}
		return reflect.ValueOf(p).Elem().Interface(), nil
	})
	r.addInputShape(name, []bool{true}, tPT, func(raw any, _ *ast.Type) (any, error) {
		if raw == nil {
			return nilPT, nil
		}
		return dec.decodeRaw(raw)
	})
	r.addInputShape(name, []bool{true, false}, tST, func(raw any, _ *ast.Type) (any, error) {
		if raw == nil {
			return nilST, nil
		}
		items := asList(raw)
		out := reflect.MakeSlice(tST, len(items), len(items))
		for i, it := range items {
			if it == nil {
				return nil, &indexedError{i, errNonNull}
			}
			p, err := dec.decodeRaw(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out.Index(i).Set(reflect.ValueOf(p).Elem())
		}
		return out.Interface(), nil
	})
	r.addInputShape(name, []bool{true, true}, tSPT, func(raw any, t *ast.Type) (any, error) {
		if raw == nil {
			return nilSPT, nil
		}
		items := asList(raw)
		out := reflect.MakeSlice(tSPT, len(items), len(items))
		for i, it := range items {
			if it == nil {
				if t.Elem.NonNull {
					return nil, &indexedError{i, errNonNull}
				}
				continue
			}
			p, err := dec.decodeRaw(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out.Index(i).Set(reflect.ValueOf(p))
		}
		return out.Interface(), nil
	})
}

// erased is a typed decoder's type-erased form.
func erased[V any](dec func(any, *ast.Type) (V, error)) func(any, *ast.Type) (any, error) {
	return func(raw any, at *ast.Type) (any, error) { return dec(raw, at) }
}

func (r *registry) addInputShape(name string, nullable []bool, t reflect.Type, dec func(any, *ast.Type) (any, error)) {
	key := typeKey{name, t}
	r.shapes[key] = shapeInfo{depth: len(nullable) - 1, nullable: nullable}
	r.decodersAny[key] = dec
}

// decodeRaw decodes one input object value into a new *T, as any.
func (d *inputDecoder) decodeRaw(raw any) (any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected an object for input %s, got %T", d.name, raw)
	}
	return d.decode(m)
}

// resolve completes the shared decoder of an Input binding once every
// registration is known.
func (ib *inputBinding) resolve(b *schemaBuilder) {
	def := b.ast.Types[ib.name]
	seen := make(map[string]bool, len(ib.fields))
	for _, f := range ib.fields {
		coord := coordinate(ib.name, f.name)
		fdef := def.Fields.ForName(f.name)
		if fdef == nil {
			b.errorf("input field %s is not defined in the schema", coord)
			continue
		}
		if seen[f.name] {
			b.errorf("input field %s is bound more than once", coord)
			continue
		}
		seen[f.name] = true
		if f.targetType != nil && f.targetType != ib.goType {
			b.errorf("input field %s: its setter writes into %s, but the input is bound to %s", coord, f.targetType, ib.goType)
			continue
		}
		set, err := f.resolve(b, fdef.Type, coord)
		if err != nil {
			b.errs = append(b.errs, fmt.Errorf("graphql: %w", err))
			continue
		}
		st, serr := newSetter(f.name, fdef.Type, fdef.DefaultValue, set)
		if serr != nil {
			b.errs = append(b.errs, fmt.Errorf("graphql: input %s: %w", coord, serr))
			continue
		}
		ib.dec.setters = append(ib.dec.setters, st)
	}
	for _, fdef := range def.Fields {
		if !seen[fdef.Name] {
			b.errorf("input field %s has no binding", coordinate(ib.name, fdef.Name))
		}
	}
	if ib.derived {
		d := ib.dec
		d.byName = make(map[string]*inputSetter, len(d.setters))
		for _, st := range d.setters {
			d.byName[st.name] = st
			if st.hasDefault || st.typ.NonNull {
				d.absent = append(d.absent, st)
			}
		}
	}
}

// defaultCycles finds input object defaults that never finish decoding. A
// value that leaves a field out is given the field's default, so a default
// that itself leaves the field out -- `input A { a: A = {} }`, or the same
// through other input objects -- is decoded without end. The specification
// forbids it and gqlparser does not check, and unbounded recursion is a stack
// overflow no recover catches.
type defaultCycles struct {
	schema *ast.Schema
	// path is the fields whose defaults are being expanded, outermost first.
	path []*inputFieldRef
	// settled fields were expanded in full, or reported already.
	settled map[*ast.FieldDefinition]bool
}

type inputFieldRef struct {
	owner string
	field *ast.FieldDefinition
}

// rejectDefaultCycles reports each cycle once, at the field that closes it,
// and returns whether there was one. Decoding any default is unsafe then, so
// the caller skips checkDefault.
func (b *schemaBuilder) rejectDefaultCycles() bool {
	c := &defaultCycles{schema: b.ast, settled: make(map[*ast.FieldDefinition]bool)}
	found := false
	for _, name := range slices.Sorted(maps.Keys(b.ast.Types)) {
		def := b.ast.Types[name]
		if def.Kind != ast.InputObject {
			continue
		}
		if closing := c.expand(def, nil); closing != nil {
			found = true
			var through []string
			for _, ref := range c.path {
				c.settled[ref.field] = true
				if len(through) > 0 || ref.field == closing.field {
					through = append(through, coordinate(ref.owner, ref.field.Name))
				}
			}
			c.path = c.path[:0]
			b.errorf("input field %s: its default %s leads back to it through %s, so a value that leaves the field out never finishes decoding",
				coordinate(closing.owner, closing.field.Name), closing.field.DefaultValue.String(), strings.Join(through, ", "))
		}
	}
	return found
}

// expand follows what decoding v as a def applies: for each field of an input
// object type, the value v supplies, or else the field's default. It returns
// the field whose default was reached while already being expanded, leaving
// path as it stood there. A nil v is the empty object.
func (c *defaultCycles) expand(def *ast.Definition, v *ast.Value) *inputFieldRef {
	if v != nil && v.Kind == ast.ListValue {
		for _, item := range v.Children {
			if closing := c.expand(def, item.Value); closing != nil {
				return closing
			}
		}
		return nil
	}
	if v != nil && v.Kind != ast.ObjectValue {
		return nil
	}
	for _, f := range def.Fields {
		fdef := c.schema.Types[f.Type.Name()]
		if fdef == nil || fdef.Kind != ast.InputObject {
			continue
		}
		if v != nil {
			if supplied := v.Children.ForName(f.Name); supplied != nil {
				if closing := c.expand(fdef, supplied); closing != nil {
					return closing
				}
				continue
			}
		}
		if f.DefaultValue == nil || c.settled[f] {
			continue
		}
		for _, ref := range c.path {
			if ref.field == f {
				return ref
			}
		}
		c.path = append(c.path, &inputFieldRef{owner: def.Name, field: f})
		if closing := c.expand(fdef, f.DefaultValue); closing != nil {
			return closing
		}
		c.path = c.path[:len(c.path)-1]
		c.settled[f] = true
	}
	return nil
}

// checkDefaults reports each field default the field's own decoder refuses.
// It runs once every input binding has resolved, because a default may hold
// another input object, and that one's decoder is complete only then.
func (ib *inputBinding) checkDefaults(b *schemaBuilder) {
	def := b.ast.Types[ib.name]
	for _, st := range ib.dec.setters {
		if err := st.checkDefault(ib.dec.newValue(), def.Fields.ForName(st.name).DefaultValue); err != nil {
			b.errorf("input field %s: %w", coordinate(ib.name, st.name), err)
		}
	}
}

// Args declares how a field's arguments are decoded into the Go struct A.
// The same A may be shared by several fields with identical arguments.
//
// It is needed only to decode an argument differently, with InputField: an A
// that FieldArgs or ResolveArgs names and no Args registers is derived from
// the struct, which is all the empty Args[A]() does.
func Args[A any](fields ...InputFieldOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		bindArgs(b, reflect.TypeFor[A](), func() any { return new(A) }, fields)
	})
}

func bindArgs(b *schemaBuilder, tA reflect.Type, newValue func() any, fields []InputFieldOption) {
	if tA.Kind() != reflect.Struct {
		b.errorf("Args[%s]: Go type must be a struct", tA)
		return
	}
	if _, dup := b.reg.argsDecoders[tA]; dup {
		b.errorf("Args[%s]: registered more than once", tA)
		return
	}
	ib := &inputBinding{goType: tA}
	for _, f := range fields {
		f.applyInput(ib)
	}
	if len(ib.fields) == 0 {
		for _, f := range autoInputFields(tA, ib.zeroForNull, nil) {
			f.applyInput(ib)
		}
		ib.derived = true
	}
	b.reg.argsDecoders[tA] = &inputDecoder{goType: tA, newValue: newValue, argsBinding: ib}
}

// argsDecoderFor is the Args binding for tA: the one an Args option
// registered, or one derived from the struct now. FieldArgs, ResolveArgs,
// SubscribeArgs and DirectiveArgs each name the struct in their own signature,
// and an empty Args[A]() adds nothing to that -- it derives every field -- so
// requiring it was a second registration to forget. The derived binding is
// held to the schema exactly as a registered one is, by build.
//
// A derived decoder makes its value through reflection, since no typed
// constructor was handed over; that is one reflect.New per decoded value, on
// a path that already reads the struct's fields reflectively.
func (b *schemaBuilder) argsDecoderFor(tA reflect.Type) (*inputDecoder, error) {
	if d := b.reg.argsDecoders[tA]; d != nil {
		return d, nil
	}
	if tA.Kind() != reflect.Struct {
		return nil, fmt.Errorf("arguments are decoded into a struct, and %s is not one", tA)
	}
	ib := &inputBinding{goType: tA, derived: true}
	for _, f := range autoInputFields(tA, false, nil) {
		f.applyInput(ib)
	}
	d := &inputDecoder{goType: tA, newValue: func() any { return reflect.New(tA).Interface() }, argsBinding: ib}
	b.reg.argsDecoders[tA] = d
	return d, nil
}

// build resolves an Args binding against the argument definitions of the
// field that uses it, producing a decoder specific to that field.
func (d *inputDecoder) build(b *schemaBuilder, args ast.ArgumentDefinitionList, coord string) (*inputDecoder, error) {
	ib := d.argsBinding
	out := &inputDecoder{goType: d.goType, newValue: d.newValue}
	if !d.copierBuilt {
		// A pointer to an input object is left to the decode: its binding
		// may have setters written by hand, and nothing here sees what they
		// made.
		d.copier = newArgsCopier(d.goType, func(t reflect.Type) bool {
			for _, in := range b.reg.inputsByName {
				if in.goType == t {
					return true
				}
			}
			return false
		})
		d.copierBuilt = true
	}
	out.copier = d.copier
	seen := make(map[string]bool, len(ib.fields))
	for _, f := range ib.fields {
		adef := args.ForName(f.name)
		if adef == nil {
			return nil, fmt.Errorf("field %s: argument %q is not defined in the schema", coord, f.name)
		}
		if seen[f.name] {
			return nil, fmt.Errorf("field %s: argument %q is bound more than once", coord, f.name)
		}
		seen[f.name] = true
		if f.targetType != nil && f.targetType != d.goType {
			return nil, fmt.Errorf("field %s: the setter for argument %q writes into %s, but the arguments are bound to %s", coord, f.name, f.targetType, d.goType)
		}
		set, err := f.resolve(b, adef.Type, coord+"("+f.name+":)")
		if err != nil {
			return nil, err
		}
		st, serr := newSetter(f.name, adef.Type, adef.DefaultValue, set)
		if serr != nil {
			return nil, fmt.Errorf("field %s: %w", coord, serr)
		}
		if !b.defaultsCycle {
			if err := st.checkDefault(d.newValue(), adef.DefaultValue); err != nil {
				return nil, fmt.Errorf("field %s: argument %q: %w", coord, f.name, err)
			}
		}
		out.setters = append(out.setters, st)
	}
	for _, adef := range args {
		if !seen[adef.Name] {
			return nil, fmt.Errorf("field %s: argument %q has no binding in Args[%s]", coord, adef.Name, d.goType)
		}
	}
	return out, nil
}

// ZeroForNull lets a Go type that cannot represent null back a nullable
// input position on this binding: absent, explicit null and the zero value
// all decode to the zero value. A null element of a list is the zero value of
// the slice's element type in the same way.
//
// The rule it relaxes is there for a reason -- a bool field cannot tell an
// omitted flag from a false one, and for a PATCH-style input those are
// different requests, which is what Omittable exists for. It is opt-in per
// binding so that the author states where the two genuinely mean the same
// thing, rather than the engine assuming it everywhere.
//
// The case it was added for is an ORM filter. entgql emits
// VariantIDIsNil bool under variantIdIsNil: Boolean and reads false as
// "apply no predicate", so absent and false really are one value; one real
// schema has 11 035 such fields, in SDL its author does not hand-write.
//
// It applies to the fields this binding derives from the struct. A field
// declared explicitly with InputField carries its own setter, so its Go type
// is already the author's choice.
func ZeroForNull() InputFieldOption { return inputFlag{zeroForNull: true} }

// InputField binds the input field or argument name to a setter on *T. V
// must be registered for the field's GraphQL type through a scalar, enum or
// input binding.
//
// A field or argument with a default has it decoded once at NewSchema, into a
// value that is discarded, so that a default the schema cannot decode is a
// build error; set runs for that as it does for a request.
func InputField[T, V any](name string, set func(*T, V)) InputFieldOption {
	return &inputFieldSpec{
		name:       name,
		valueType:  reflect.TypeFor[V](),
		targetType: reflect.TypeFor[T](),
		resolve: func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error) {
			dec, err := inputDecoderFor[V](b.reg, typ, coord)
			if err != nil {
				return nil, err
			}
			return func(target, raw any) error {
				v, err := dec(raw, typ)
				if err != nil {
					return err
				}
				set(target.(*T), v)
				return nil
			}, nil
		},
	}
}

// OmittableField is like InputField but the setter receives an Omittable
// that is set whenever the field was present or a default applied, even if
// its value is null.
func OmittableField[T, V any](name string, set func(*T, Omittable[V])) InputFieldOption {
	return &inputFieldSpec{
		name:       name,
		valueType:  reflect.TypeFor[V](),
		targetType: reflect.TypeFor[T](),
		resolve: func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error) {
			dec, err := inputDecoderFor[V](b.reg, typ, coord)
			if err != nil {
				return nil, err
			}
			return func(target, raw any) error {
				v, err := dec(raw, typ)
				if err != nil {
					return err
				}
				set(target.(*T), OmittableOf(v))
				return nil
			}, nil
		},
	}
}

// inputDecoderFor looks up the typed decoder for Go type V against SDL type
// typ and validates the shape.
func inputDecoderFor[V any](r *registry, typ *ast.Type, coord string) (func(any, *ast.Type) (V, error), error) {
	key := typeKey{typ.Name(), reflect.TypeFor[V]()}
	if err := checkInputShape(r, key, typ, false); err != nil {
		return nil, fmt.Errorf("input %s: %w", coord, err)
	}
	if typed, ok := r.decoders[key].(func(any, *ast.Type) (V, error)); ok {
		return typed, nil
	}
	// An input object's shapes are registered type-erased.
	dec := r.decodersAny[key]
	return func(raw any, t *ast.Type) (V, error) {
		v, err := dec(raw, t)
		if err != nil {
			var zero V
			return zero, err
		}
		return v.(V), nil
	}, nil
}

var omittablePkg = reflect.TypeFor[Omittable[int]]().PkgPath()

// autoInputFields derives InputField / OmittableField bindings from exported
// struct fields. Names come from a `graphql` tag, else a `json` tag, else
// a GraphQL-style lowerCamel conversion (AuthorID → authorId).
//
// declared is the input object's field names, and is nil for Args, whose
// arguments are only known per field (inputDecoder.build). When it is present
// a serialization tag that names nothing in the schema falls back to the Go
// field name: an ORM writes `json:"role_id"` on RoleID under a schema that
// declares roleId, and the tag is describing its wire format, not this one.
// The fallback can only turn a build error into a binding -- a tag naming a
// field the schema does not declare is rejected -- and it never takes a name
// another field's tag already claimed.
func autoInputFields(t reflect.Type, zeroForNull bool, declared map[string]bool) []InputFieldOption {
	type derived struct {
		index   []int
		sf      reflect.StructField
		name    string
		fromTag bool
	}
	var fields []derived
	claimed := make(map[string]bool)
	walkStructFields(t, nil, func(index []int, sf reflect.StructField) {
		name, fromTag, skip := graphqlNameOf(sf)
		if skip || name == "" {
			return
		}
		fields = append(fields, derived{append([]int(nil), index...), sf, name, fromTag})
		if declared[name] {
			claimed[name] = true
		}
	})
	out := make([]InputFieldOption, 0, len(fields))
	for _, f := range fields {
		name := f.name
		if f.fromTag && declared != nil && !declared[name] {
			if alt := exportedToGraphQL(f.sf.Name); declared[alt] && !claimed[alt] {
				name, claimed[alt] = alt, true
			}
		}
		elem, omittable := unwrapOmittable(f.sf.Type)
		out = append(out, &inputFieldSpec{
			name:      name,
			valueType: elem,
			resolve:   autoSetter(f.index, elem, omittable, zeroForNull),
		})
	}
	return out
}

func autoSetter(index []int, valueType reflect.Type, omittable bool, zeroForNull bool) func(*schemaBuilder, *ast.Type, string) (func(target, raw any) error, error) {
	return func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error) {
		key := typeKey{typ.Name(), valueType}
		if err := checkInputShape(b.reg, key, typ, zeroForNull); err != nil {
			return nil, fmt.Errorf("input %s: %w", coord, err)
		}
		dec := b.reg.decodersAny[key]
		if dec == nil {
			return nil, fmt.Errorf("input %s: no decoder for %s as %s", coord, valueType, typ.Name())
		}
		// A relaxed shape check is only half of it: the decoder for a Go type
		// that cannot be null still refuses one, so an explicit null has to be
		// answered here, before it reaches the decoder, with the same zero value
		// an absent field leaves behind. The check is relaxed at every list
		// level, so a null element is answered the same way.
		nullIsZero := zeroForNull && !typ.NonNull
		if zeroForNull && nullBelowNeedsZero(b.reg.shapes[key], typ) {
			dec = b.reg.zeroingListDecoder(key, typ)
		}
		return func(target, raw any) error {
			if raw == nil && nullIsZero {
				fv := reflect.ValueOf(target).Elem().FieldByIndex(index)
				if omittable {
					slot := reflect.New(fv.Type())
					slot.Interface().(omittableAssigner).assign(nil, true)
					fv.Set(slot.Elem())
					return nil
				}
				fv.SetZero()
				return nil
			}
			v, err := dec(raw, typ)
			if err != nil {
				return err
			}
			fv := reflect.ValueOf(target).Elem().FieldByIndex(index)
			if omittable {
				slot := reflect.New(fv.Type())
				slot.Interface().(omittableAssigner).assign(v, true)
				fv.Set(slot.Elem())
				return nil
			}
			if v == nil {
				fv.SetZero()
				return nil
			}
			fv.Set(reflect.ValueOf(v))
			return nil
		}, nil
	}
}

// nullBelowNeedsZero reports whether sdl lets an element be null where the Go
// shape cannot hold one, which only a ZeroForNull binding builds with.
func nullBelowNeedsZero(info shapeInfo, sdl *ast.Type) bool {
	level := 1
	for t := sdl.Elem; t != nil; t = t.Elem {
		if !t.NonNull && !info.nullable[level] {
			return true
		}
		level++
	}
	return false
}

// zeroingListDecoder decodes the list shape key as its registered decoder
// does, except that a null element at a nullable position is left as the zero
// value of the Go element type rather than handed to a decoder that refuses
// it. The registered decoder is tried first and answers every list that holds
// no such null, so only a list that does is built again here, reflectively.
func (r *registry) zeroingListDecoder(key typeKey, sdl *ast.Type) func(any, *ast.Type) (any, error) {
	whole := r.decodersAny[key]
	if sdl.Elem == nil {
		return whole
	}
	elem := r.zeroingListDecoder(typeKey{key.name, key.typ.Elem()}, sdl.Elem)
	return func(raw any, at *ast.Type) (any, error) {
		if v, err := whole(raw, at); err == nil || raw == nil {
			return v, err
		}
		items := asList(raw)
		out := reflect.MakeSlice(key.typ, len(items), len(items))
		for i, it := range items {
			if it == nil && !at.Elem.NonNull {
				continue
			}
			v, err := elem(it, at.Elem)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			if v != nil {
				out.Index(i).Set(reflect.ValueOf(v))
			}
		}
		return out.Interface(), nil
	}
}

func walkStructFields(t reflect.Type, prefix []int, yield func([]int, reflect.StructField)) {
	for i := range t.NumField() {
		sf := t.Field(i)
		index := append(append([]int(nil), prefix...), i)
		if sf.Anonymous {
			et := sf.Type
			if et.Kind() == reflect.Struct {
				walkStructFields(et, index, yield)
				continue
			}
		}
		if sf.IsExported() {
			yield(index, sf)
		}
	}
}

func graphqlNameOf(sf reflect.StructField) (name string, fromTag, skip bool) {
	if tag, ok := sf.Tag.Lookup("graphql"); ok {
		if tag == "-" {
			return "", false, true
		}
		name, _, _ = strings.Cut(tag, ",")
		return name, true, name == ""
	}
	if tag, ok := sf.Tag.Lookup("json"); ok {
		// json:"-" means the field is not on the wire, and it has to mean the
		// same here or the name is derived from the Go field instead and the
		// input fails against a schema that never declared it. An ORM marks
		// its internal fields this way: one real WhereInput carries
		// Predicates []predicate.X `json:"-"`, and deriving "predicates" from
		// it was 447 build errors on a schema whose author had said, in the
		// one way Go has of saying it, that the field is not input.
		// json:"-," is encoding/json's escape for a field really named "-".
		if tag == "-" {
			return "", false, true
		}
		name, opt, _ := strings.Cut(tag, ",")
		if name != "" && !strings.Contains(opt, "inline") {
			return name, true, false
		}
	}
	return exportedToGraphQL(sf.Name), false, false
}

// initialisms are the words a Go name writes in capitals and a GraphQL name
// does not. UUID and GUID come first so that each is taken whole: they end in
// ID, and read as a word followed by one they became uUId and gUId.
var initialisms = strings.NewReplacer("UUID", "Uuid", "GUID", "Guid", "ID", "Id", "URL", "Url")

// exportedToGraphQL maps AuthorID → authorId, URL → url and UUID → uuid.
func exportedToGraphQL(name string) string {
	if name == "" {
		return name
	}
	name = initialisms.Replace(name)
	r, w := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(r)) + name[w:]
}

func unwrapOmittable(t reflect.Type) (reflect.Type, bool) {
	if t.PkgPath() == omittablePkg && strings.HasPrefix(t.Name(), "Omittable") {
		return t.Field(0).Type, true
	}
	return t, false
}
