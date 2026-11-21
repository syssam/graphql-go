package codegen

import "maps"

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
// redeclaration, not a warning, in a file marked DO NOT EDIT. Any other path
// is given a qualifier by the rule mapped packages follow (pathQualifier),
// against every name either table holds, and is always imported under it as an
// alias: the path's last element is neither a package's name when the two
// differ (.../client/method under package methodclient) nor one at all when it
// is a major version.
func (b *builder) extraQualifier(path string) string {
	if q, ok := b.modelExprQualifier(path); ok {
		return q
	}
	for q, have := range b.extraImports {
		if have == path {
			return q
		}
	}
	taken := maps.Clone(b.modelExprImports())
	maps.Copy(taken, b.extraImports)
	q := pathQualifier(path, taken)
	b.extraImports[q] = path
	return q
}
