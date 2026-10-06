package codegen

import (
	"strconv"
	"strings"
)

// extraEnum is one second binding for an SDL enum, already qualified: ref is
// the Go type as generated code writes it, and consts maps each SDL value to
// the constant reference that carries it.
type extraEnum struct {
	ref    string
	consts map[string]string
}

// setExtraEnums renders Manifest.ExtraEnums into the references generated code
// will carry, and reserves an import qualifier for each package they come
// from. The qualifier is settled here, once, because the emitter writes the
// reference and the import block from the same two maps -- deciding it twice
// is how a reference and its import disagree.
func (b *builder) setExtraEnums(man *Manifest) {
	if man == nil || len(man.ExtraEnums) == 0 {
		return
	}
	b.extraEnums = map[string][]extraEnum{}
	b.extraImports = map[string]string{}
	for _, tb := range man.ExtraEnums {
		if tb.Go.zero() || len(tb.Values) == 0 {
			continue
		}
		q := b.extraQualifier(tb.Go.PkgPath)
		e := extraEnum{ref: q + "." + tb.Go.Name, consts: map[string]string{}}
		for value, ident := range tb.Values {
			e.consts[value] = q + "." + ident
		}
		b.extraEnums[tb.Name] = append(b.extraEnums[tb.Name], e)
	}
}

// extraQualifier picks the name an extra enum's package is imported under, and
// registers the import unless one already covers it.
//
// A package Config.Models already names is imported by that path, under that
// qualifier, so registering it again emits the import twice -- which is a
// redeclaration, not a warning, in a file marked DO NOT EDIT. A name spoken
// for by a *different* package gets a suffix instead of losing, because both
// have to be referable at once.
func (b *builder) extraQualifier(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ReplaceAll(base, "-", "")
	if base == "" {
		base = "enum"
	}
	for q, have := range b.extraImports {
		if have == path {
			return q
		}
	}
	for q, have := range b.modelExprImports() {
		if have == path {
			return q
		}
	}
	taken := func(q string) bool {
		if reservedQualifier(q) {
			return true
		}
		if p, ok := b.modelExprImports()[q]; ok && p != path {
			return true
		}
		_, ok := b.extraImports[q]
		return ok
	}
	q := base
	for i := 2; taken(q); i++ {
		q = base + "enum"
		if i > 2 {
			q = base + "enum" + strconv.Itoa(i)
		}
	}
	b.extraImports[q] = path
	return q
}
