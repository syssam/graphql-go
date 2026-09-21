package codegen

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
)

func (b *builder) sdlFile(typeName string) string {
	def := b.schema.Types[typeName]
	if def == nil || def.Position == nil || def.Position.Src == nil {
		return ""
	}
	return def.Position.Src.Name
}

func (b *builder) groupOf(typeName string) string {
	if b.manifest != nil {
		if g, ok := b.manifest.groups[typeName]; ok {
			return sanitizeGroup(g, b.pkgName)
		}
	}
	file := b.sdlFile(typeName)
	if b.cfg.GroupFunc != nil {
		if g := b.cfg.GroupFunc(typeName, file); g != "" {
			return sanitizeGroup(g, b.pkgName)
		}
	}
	return sanitizeGroup(fileStem(file), b.pkgName)
}

func (b *builder) inGroup(typeName, group string) bool {
	return group == "" || b.groupOf(typeName) == group
}

func (b *builder) uniqueGroups() []string {
	seen := map[string]bool{}
	for _, kind := range []ast.DefinitionKind{ast.Object, ast.InputObject, ast.Enum, ast.Scalar, ast.Interface, ast.Union} {
		for _, name := range b.typeNames(kind) {
			if !b.groupEmits(name) {
				continue
			}
			seen[b.groupOf(name)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}

func (b *builder) groupEmits(typeName string) bool {
	def := b.schema.Types[typeName]
	if def == nil || def.BuiltIn {
		return false
	}
	if def.Kind == ast.Scalar {
		_, mapped := b.cfg.Models[typeName]
		return !mapped
	}
	return true
}

func (b *builder) hasResolver(group string) bool {
	for _, name := range b.typeNames(ast.Object) {
		if !b.inGroup(name, group) {
			continue
		}
		def := b.schema.Types[name]
		for _, fd := range def.Fields {
			if strings.HasPrefix(fd.Name, "__") {
				continue
			}
			if b.fieldKind(name, fd) == fieldResolve {
				return true
			}
		}
	}
	return false
}

func fileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func sanitizeGroup(name, pkgName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || !unicode.IsLetter(rune(s[0])) {
		if s == "" {
			s = "types"
		} else {
			s = "g" + s
		}
	}
	switch s {
	case "schema":
		s = "types"
	case "model", "internal":
		s += "grp"
	default:
		if s == pkgName {
			s += "grp"
		}
	}
	return s
}

func groupField(name string) string {
	r, w := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r)) + name[w:]
}

// modelPkgNames lists the model packages this schema emits.
func (b *builder) modelPkgNames() []string {
	if !b.modelSplit {
		return []string{"model"}
	}
	return b.uniqueGroups()
}

// modelGroupsAcyclic reports whether per-group model packages could import one
// another without forming a cycle, which Go forbids.
//
// Generated models hold only leaf fields -- composite fields become resolver
// methods -- so object types never reference each other and cannot create a
// cycle. Input objects can reference other input objects, so two groups whose
// inputs refer to each other must stay in one package.
func (b *builder) modelGroupsAcyclic(groups []string) bool {
	deps := map[string]map[string]bool{}
	edge := func(ownerType, refType string) {
		from, to := b.groupOf(ownerType), b.groupOf(refType)
		if from == to {
			return
		}
		if deps[from] == nil {
			deps[from] = map[string]bool{}
		}
		deps[from][to] = true
	}
	ref := func(ownerType string, t *ast.Type) {
		named := t.Name()
		def := b.schema.Types[named]
		if def == nil || def.BuiltIn {
			return
		}
		if _, mapped := b.cfg.Models[named]; mapped {
			return
		}
		if def.Kind == ast.Interface || def.Kind == ast.Union {
			return
		}
		edge(ownerType, named)
	}

	for _, name := range b.typeNames(ast.Object) {
		if b.isRoot(name) {
			continue
		}
		for _, fd := range b.schema.Types[name].Fields {
			if b.fieldKind(name, fd) == fieldPure {
				ref(name, fd.Type)
			}
		}
	}
	for _, name := range b.typeNames(ast.InputObject) {
		for _, fd := range b.schema.Types[name].Fields {
			ref(name, fd.Type)
		}
	}

	const (
		white = iota
		grey
		black
	)
	colour := map[string]int{}
	var visit func(string) bool
	visit = func(g string) bool {
		colour[g] = grey
		for next := range deps[g] {
			switch colour[next] {
			case grey:
				return false
			case white:
				if !visit(next) {
					return false
				}
			}
		}
		colour[g] = black
		return true
	}
	for _, g := range groups {
		if colour[g] == white && !visit(g) {
			return false
		}
	}
	return true
}
