package codegen

import (
	"fmt"
	"strconv"
	"strings"
)

// wireCall is what wire.go passes a group's constructor.
//
// Wire.Pass says, where it has an entry, and is believed over everything
// below. Otherwise what an existing constructor takes decides: one that takes
// the dependencies type is passed it; one parameter named after a field is
// passed that field; any other single parameter is passed the group's field
// if there is one, whether or not gqlc could have written that field's type
// itself, since nothing is being written; and none, or several, are decided
// by wireArgs. For a constructor still to be scaffolded the struct decides,
// as the scaffold will.
func (b *builder) wireCall(name, target, dir, typ, implImport string) (string, error) {
	depsPath, depsQual, depsType := b.wireDeps()
	ctor := constructorName(typ)
	existing := b.existingConstructor(dir, ctor)
	pass, err := b.wirePass(name, target)
	switch {
	case err != nil:
		return "", err
	case pass != nil && existing == nil && pass.named != "":
		// The scaffold will write this constructor and has to spell the
		// field's type in the implementation's package. Whether it can is
		// found out here, before anything is on disk.
		switch {
		case pass.field == nil && b.wire.structRead:
			return "", fmt.Errorf("codegen: wire.pass: %s: %s has no %s yet, and gqlc cannot write one that takes %s: %s declares no field %s, and what it embeds is not read; write the constructor yourself",
				pass.from, target, ctor, pass.expr, depsType, pass.named)
		case pass.field == nil:
			return "", fmt.Errorf("codegen: wire.pass: %s: %s has no %s yet, and gqlc cannot read %s to write one that takes %s; write the constructor yourself",
				pass.from, target, ctor, b.cfg.Wire.Deps, pass.expr)
		}
		if _, err := pass.field.placed(depsPath, depsQual, implImport, func(_, preferred string) string { return preferred }); err != nil {
			return "", fmt.Errorf("codegen: wire.pass: %s: %s has no %s yet, and gqlc cannot write one that takes %s (%w); write the constructor yourself",
				pass.from, target, ctor, pass.expr, err)
		}
		return pass.expr, nil
	case pass != nil:
		return pass.expr, nil
	case existing == nil || existing.unknown:
		// As it was before such a constructor was known to be there: a group
		// with no entry is decided exactly as it would be without Wire.Pass.
		if f, _ := b.wireArg(name, target); f != nil {
			return "d." + f.name, nil
		}
		return "d", nil
	}
	if len(existing.params) != 1 {
		return b.wireArgs(target, ctor, existing, dir, depsPath, depsType, implImport)
	}
	whole, pointer := existing.takesType(existing.params[0].typ, depsPath, depsType, implImport, b.wire.ctors[dir])
	switch {
	case whole && pointer:
		return "&d", nil
	case whole:
		return "d", nil
	}
	// The parameter's name, where it names a field, is the author saying which
	// one: a group whose one service is called something else than the group
	// had no other way to say so but an entry in wire.pass. Not scaffoldParam,
	// which is nobody saying anything: it is what gqlc calls the parameter of
	// every constructor it writes, whichever field that takes.
	if p := existing.params[0]; p.name != scaffoldParam {
		if f := b.paramField(p); f != nil {
			return "d." + f.name, nil
		}
	}
	if f := b.wire.fields[foldName(name)]; f != nil && !f.twice && !b.wireShared(target) {
		return "d." + f.name, nil
	}
	return "d", nil
}

// scaffoldParam is what a constructor gqlc scaffolds calls the field it is
// given. A scaffolded constructor is read back on every later run as one that
// exists, so the name must not be taken for a field's: with a Dep field in the
// dependencies, the run after the one that wrote NewHandler(dep *user.Service)
// passed it d.Dep. The scaffold writes this name and wireCall sets it aside,
// from one constant, so the two cannot come apart.
const scaffoldParam = "dep"

// paramField is the field of the dependencies a parameter is named after, or
// nil when its name is no field's, or is two fields' once case and
// punctuation are set aside.
func (b *builder) paramField(p ctorParam) *depsField {
	if p.name == "" || p.name == "_" {
		return nil
	}
	if f := b.wire.fields[foldName(p.name)]; f != nil && !f.twice {
		return f
	}
	return nil
}

// wireArgs is what wire.go passes a constructor that does not take exactly
// one thing.
//
// One that takes nothing is called with nothing: a group whose methods need
// no dependencies has no reason to import them, and passing it the whole
// value was a compile error. One that takes several is given, for each
// parameter, the field of the dependencies named after it, so a group that
// calls three services imports those three and not every one the struct
// holds; a parameter of the dependencies type itself is given the whole
// value. Nothing else can be meant, so anything else is refused here rather
// than written into a file nobody may edit: a parameter with no name, a name
// no field has or two fields fold to, and a variadic parameter.
func (b *builder) wireArgs(target, ctor string, c *constructor, dir, depsPath, depsType, implImport string) (string, error) {
	args := make([]string, 0, len(c.params))
	for i, p := range c.params {
		which := p.name
		if which == "" || which == "_" {
			which = strconv.Itoa(i + 1)
		}
		if strings.HasPrefix(p.typ, "...") {
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s is variadic, and Wire gives each parameter one field of %s", target, which, ctor, depsType)
		}
		whole, pointer := c.takesType(p.typ, depsPath, depsType, implImport, b.wire.ctors[dir])
		switch {
		case whole && pointer:
			args = append(args, "&d")
			continue
		case whole:
			args = append(args, "d")
			continue
		}
		if p.name == "" || p.name == "_" {
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s has no name, and Wire gives a parameter the field of %s it is named after", target, which, ctor, depsType)
		}
		f := b.wire.fields[foldName(p.name)]
		switch {
		case f != nil && f.twice:
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s could be either of two fields of %s whose names differ only in case or punctuation; rename one", target, p.name, ctor, depsType)
		case f != nil:
			args = append(args, "d."+f.name)
		case !b.wire.structRead:
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s is matched to a field of %s by name, and gqlc cannot read %s to find it", target, p.name, ctor, depsType, b.cfg.Wire.Deps)
		case b.wire.embeds:
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s names no field %s declares, and fields promoted from what it embeds are not read; declare the field in %s itself", target, p.name, ctor, depsType, depsType)
		default:
			return "", fmt.Errorf("codegen: wire: %s: parameter %s of %s names no field of %s; name it after the field it takes", target, p.name, ctor, depsType)
		}
	}
	return strings.Join(args, ", "), nil
}

// wireArg is the field of the dependencies a new constructor for a group is
// given, or nil when it is given the whole value. The error says why a field
// that is there is not handed over.
//
// A constructor taking the whole struct makes its package import everything
// the struct names. With a service per group that is every service, so every
// group's package recompiles when any one of them changes: at 300 groups, a
// method added to one service rebuilt 305 packages and took 70 s of CPU,
// against 6 packages when each group imports its own service alone. So a
// field named after the group is what the group is given. A group with no
// such field gets the whole value, and so does a type implementing several
// groups, whose one constructor cannot take a field for each.
func (b *builder) wireArg(name, target string) (*depsField, error) {
	b.readWire()
	if b.wireShared(target) {
		return nil, nil
	}
	// A target that is not dir.Type is refused by whoever reads it first.
	dir, _, err := b.splitTarget(target)
	if err != nil {
		return nil, err
	}
	f := b.wire.fields[foldName(name)]
	if f == nil {
		return nil, nil
	}
	depsPath, depsQual, depsType := b.wireDeps()
	self, _ := b.importPathOf(dir)
	if _, err := f.placed(depsPath, depsQual, self, func(_, preferred string) string { return preferred }); err != nil {
		return nil, fmt.Errorf("%s.%s is not handed to the %s group on its own: %w", depsType, f.name, name, err)
	}
	return f, nil
}

// wireAnyField reports whether any group is handed a field of its own, which
// is what makes one that is not worth a word.
func (b *builder) wireAnyField() bool {
	if b.wire.anyField == nil {
		found := false
		for name, target := range b.cfg.Scaffold {
			// What a group is actually given: its entry's, where it has one.
			if p, err := b.wirePass(name, target); err == nil && p != nil {
				if p.named != "" {
					found = true
					break
				}
				continue
			}
			if f, _ := b.wireArg(name, target); f != nil {
				found = true
				break
			}
		}
		b.wire.anyField = &found
	}
	return *b.wire.anyField
}
