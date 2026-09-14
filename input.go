package graphql

import (
	"fmt"
	"reflect"
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
}

// inputFieldSpec is an input field binding before it is resolved against the
// SDL definition of the field or argument.
type inputFieldSpec struct {
	name      string
	valueType reflect.Type
	resolve   func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error)
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

// decode applies every setter to a freshly allocated *T. Absent fields fall
// back to their SDL default; absent nullable fields without a default are
// left at their zero value and, for OmittableField, remain unset.
func (d *inputDecoder) decode(m map[string]any) (any, error) {
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
		def := b.ast.Types[name]
		if def == nil || def.Kind != ast.InputObject {
			b.errorf("Input %q: type is not an input object in the schema", name)
			return
		}
		tT := reflect.TypeFor[T]()
		if tT.Kind() != reflect.Struct {
			b.errorf("Input %q: Go type %s must be a struct", name, tT)
			return
		}
		if prev := b.reg.inputsByName[name]; prev != nil {
			b.errorf("Input %q: already bound to %s", name, prev.goType)
			return
		}
		dec := &inputDecoder{name: name, goType: tT, newValue: func() any { return new(T) }}
		ib := &inputBinding{name: name, goType: tT, dec: dec}
		if len(fields) == 0 {
			fields = autoInputFields(tT)
		}
		for _, f := range fields {
			f.applyInput(ib)
		}
		b.inputs = append(b.inputs, ib)
		b.reg.inputsByName[name] = dec
		registerInputShapes[T](b.reg, name, dec)
	})
}

// registerInputShapes registers decoders for T, *T, []T and []*T that all
// route through dec, which is completed later by inputBinding.resolve.
func registerInputShapes[T any](r *registry, name string, dec *inputDecoder) {
	decodeOne := func(raw any) (*T, error) {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected an object for input %s, got %T", name, raw)
		}
		v, err := dec.decode(m)
		if err != nil {
			return nil, err
		}
		return v.(*T), nil
	}
	tT := reflect.TypeFor[T]()
	tPT := reflect.TypeFor[*T]()
	tST := reflect.TypeFor[[]T]()
	tSPT := reflect.TypeFor[[]*T]()

	r.shapes[typeKey{name, tT}] = shapeInfo{depth: 0, nullable: []bool{false}}
	r.shapes[typeKey{name, tPT}] = shapeInfo{depth: 0, nullable: []bool{true}}
	r.shapes[typeKey{name, tST}] = shapeInfo{depth: 1, nullable: []bool{true, false}}
	r.shapes[typeKey{name, tSPT}] = shapeInfo{depth: 1, nullable: []bool{true, true}}

	setDecoder(r, name, tT, func(raw any, _ *ast.Type) (T, error) {
		if raw == nil {
			var zero T
			return zero, errNonNull
		}
		p, err := decodeOne(raw)
		if err != nil {
			var zero T
			return zero, err
		}
		return *p, nil
	})
	setDecoder(r, name, tPT, func(raw any, _ *ast.Type) (*T, error) {
		if raw == nil {
			return nil, nil
		}
		return decodeOne(raw)
	})
	setDecoder(r, name, tST, func(raw any, _ *ast.Type) ([]T, error) {
		if raw == nil {
			return nil, nil
		}
		items := asList(raw)
		out := make([]T, len(items))
		for i, it := range items {
			if it == nil {
				return nil, &indexedError{i, errNonNull}
			}
			p, err := decodeOne(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out[i] = *p
		}
		return out, nil
	})
	setDecoder(r, name, tSPT, func(raw any, t *ast.Type) ([]*T, error) {
		if raw == nil {
			return nil, nil
		}
		items := asList(raw)
		out := make([]*T, len(items))
		for i, it := range items {
			if it == nil {
				if t.Elem.NonNull {
					return nil, &indexedError{i, errNonNull}
				}
				continue
			}
			p, err := decodeOne(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out[i] = p
		}
		return out, nil
	})
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
}

// Args declares how a field's arguments are decoded into the Go struct A.
// The same A may be shared by several fields with identical arguments.
func Args[A any](fields ...InputFieldOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		tA := reflect.TypeFor[A]()
		if tA.Kind() != reflect.Struct {
			b.errorf("Args[%s]: Go type must be a struct", tA)
			return
		}
		if _, dup := b.reg.argsDecoders[tA]; dup {
			b.errorf("Args[%s]: registered more than once", tA)
			return
		}
		ib := &inputBinding{goType: tA}
		if len(fields) == 0 {
			fields = autoInputFields(tA)
		}
		for _, f := range fields {
			f.applyInput(ib)
		}
		b.reg.argsDecoders[tA] = &inputDecoder{goType: tA, newValue: func() any { return new(A) }, argsBinding: ib}
	})
}

// build resolves an Args binding against the argument definitions of the
// field that uses it, producing a decoder specific to that field.
func (d *inputDecoder) build(b *schemaBuilder, args ast.ArgumentDefinitionList, coord string) (*inputDecoder, error) {
	ib := d.argsBinding
	out := &inputDecoder{goType: d.goType, newValue: d.newValue}
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
		set, err := f.resolve(b, adef.Type, coord+"("+f.name+":)")
		if err != nil {
			return nil, err
		}
		st, serr := newSetter(f.name, adef.Type, adef.DefaultValue, set)
		if serr != nil {
			return nil, fmt.Errorf("field %s: %w", coord, serr)
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

// InputField binds the input field or argument name to a setter on *T. V
// must be registered for the field's GraphQL type through a scalar, enum or
// input binding.
func InputField[T, V any](name string, set func(*T, V)) InputFieldOption {
	return &inputFieldSpec{
		name:      name,
		valueType: reflect.TypeFor[V](),
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
		name:      name,
		valueType: reflect.TypeFor[V](),
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
	if err := checkInputShape(r, key, typ); err != nil {
		return nil, fmt.Errorf("input %s: %w", coord, err)
	}
	return r.decoders[key].(func(any, *ast.Type) (V, error)), nil
}

var omittablePkg = reflect.TypeFor[Omittable[int]]().PkgPath()

// autoInputFields derives InputField / OmittableField bindings from exported
// struct fields. Names come from a `graphql` tag, else a `json` tag, else
// a GraphQL-style lowerCamel conversion (AuthorID → authorId).
func autoInputFields(t reflect.Type) []InputFieldOption {
	var out []InputFieldOption
	walkStructFields(t, nil, func(index []int, sf reflect.StructField) {
		name, skip := graphqlNameOf(sf)
		if skip || name == "" {
			return
		}
		idx := append([]int(nil), index...)
		elem, omittable := unwrapOmittable(sf.Type)
		out = append(out, &inputFieldSpec{
			name:      name,
			valueType: elem,
			resolve:   autoSetter(idx, elem, omittable),
		})
	})
	return out
}

func autoSetter(index []int, valueType reflect.Type, omittable bool) func(*schemaBuilder, *ast.Type, string) (func(target, raw any) error, error) {
	return func(b *schemaBuilder, typ *ast.Type, coord string) (func(target, raw any) error, error) {
		key := typeKey{typ.Name(), valueType}
		if err := checkInputShape(b.reg, key, typ); err != nil {
			return nil, fmt.Errorf("input %s: %w", coord, err)
		}
		dec := b.reg.decodersAny[key]
		if dec == nil {
			return nil, fmt.Errorf("input %s: no decoder for %s as %s", coord, valueType, typ.Name())
		}
		return func(target, raw any) error {
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

func graphqlNameOf(sf reflect.StructField) (name string, skip bool) {
	if tag, ok := sf.Tag.Lookup("graphql"); ok {
		if tag == "-" {
			return "", true
		}
		name, _, _ = strings.Cut(tag, ",")
		return name, name == ""
	}
	if tag, ok := sf.Tag.Lookup("json"); ok && tag != "-" {
		name, opt, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" && !strings.Contains(opt, "inline") {
			return name, false
		}
	}
	return exportedToGraphQL(sf.Name), false
}

// exportedToGraphQL maps AuthorID → authorId and URL → url.
func exportedToGraphQL(name string) string {
	if name == "" {
		return name
	}
	name = strings.ReplaceAll(name, "ID", "Id")
	name = strings.ReplaceAll(name, "URL", "Url")
	r, w := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(r)) + name[w:]
}

func unwrapOmittable(t reflect.Type) (reflect.Type, bool) {
	if t.PkgPath() == omittablePkg && strings.HasPrefix(t.Name(), "Omittable") {
		return t.Field(0).Type, true
	}
	return t, false
}
