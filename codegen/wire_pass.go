package codegen

import (
	"fmt"
	goast "go/ast"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// passed is what Wire.Pass says a group's constructor is given.
type passed struct {
	// expr is the entry, which is what wire.go writes, and from the group it
	// is written under, which for a type implementing several need not be
	// the group being decided.
	expr string
	from string
	// field is the field it names, or nil for the whole value; also nil for
	// a field of a struct this run could not read.
	field   *depsField
	named   string
	pointer bool
}

// wirePass is what Wire.Pass says the group's constructor is given, or nil
// when it says nothing.
//
// It is the way out of a wrong reading. What a constructor is given is
// otherwise worked out from source, and when that is wrong the mistake is in
// wire.go, which nobody may edit. So an entry is believed -- nothing about
// the constructor or the field's type is weighed against it -- and is checked
// only for being something that could be meant, and only as far as this run
// can see: a field is looked for where the struct can be read and embeds
// nothing, since an embedded type's fields are the struct's too and are not
// listed here.
//
// A type implementing several groups has one constructor and is built once,
// so one entry speaks for every group it serves, whichever it is written
// under; two that differ are refused.
func (b *builder) wirePass(name, target string) (*passed, error) {
	b.readWire()
	entry, ok := b.cfg.Wire.Pass[name]
	from, key := name, b.targetKey(target)
	for _, other := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		theirs, has := b.cfg.Wire.Pass[other]
		if !has || other == name || b.targetKey(b.cfg.Scaffold[other]) != key {
			continue
		}
		if !ok {
			entry, from, ok = theirs, other, true
			continue
		}
		if strings.TrimSpace(theirs) != strings.TrimSpace(entry) {
			one, two := [2]string{from, strings.TrimSpace(entry)}, [2]string{other, strings.TrimSpace(theirs)}
			if two[0] < one[0] {
				one, two = two, one
			}
			return nil, fmt.Errorf("codegen: wire.pass: %s and %s are implemented by one type, which is built once from one thing, and are given %s and %s; say it once",
				one[0], two[0], one[1], two[1])
		}
	}
	if !ok {
		return nil, nil
	}
	_, _, depsType := b.wireDeps()
	p := &passed{expr: strings.TrimSpace(entry), from: from}
	if p.expr == "" {
		return nil, fmt.Errorf("codegen: wire.pass: %s: the entry is empty; in YAML an unquoted &d is an anchor and not a value, so quote it: \"&d\"", from)
	}
	rest := strings.TrimPrefix(p.expr, "&")
	p.pointer = rest != p.expr
	if rest == "d" {
		return p, nil
	}
	field, isField := strings.CutPrefix(rest, "d.")
	if !isField || !token.IsIdentifier(field) || field == "_" {
		return nil, fmt.Errorf("codegen: wire.pass: %s: %q is not what a constructor can be given; write d, &d, d.Field or &d.Field", from, entry)
	}
	p.named = field
	// wire.go is in the generated package, so that is where an unexported
	// field has to be. The name is all it takes to know, whether or not the
	// struct could be read, and whether it is declared or promoted.
	inPackage := b.cfg.Wire.Deps[:strings.LastIndex(b.cfg.Wire.Deps, ".")] == b.cfg.Package
	if !goast.IsExported(field) && !inPackage {
		return nil, fmt.Errorf("codegen: wire.pass: %s: %s.%s is not exported, and wire.go is in another package", from, depsType, field)
	}
	if !b.wire.structRead {
		return p, nil
	}
	p.field = b.wire.named[field]
	switch {
	case p.field == nil && b.wire.embeds:
		// Promoted from something embedded, perhaps; the compiler will say.
		return p, nil
	case p.field == nil:
		var have []string
		for n := range b.wire.named {
			if n != "_" && (inPackage || goast.IsExported(n)) {
				have = append(have, n)
			}
		}
		slices.Sort(have)
		return nil, fmt.Errorf("codegen: wire.pass: %s: %s has no field %s; it has %s", from, depsType, field, strings.Join(have, ", "))
	}
	// Named exactly, so that another field folds to the same name is no
	// longer a reason not to know which is meant.
	exact := *p.field
	exact.twice = false
	p.field = &exact
	return p, nil
}
