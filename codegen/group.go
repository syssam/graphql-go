package codegen

import (
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

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
	if g, ok := b.groupByType[typeName]; ok {
		return g
	}
	g := b.groupOfUncached(typeName)
	if b.groupByType == nil {
		b.groupByType = make(map[string]string, len(b.schema.Types))
	}
	b.groupByType[typeName] = g
	return g
}

// groupOfUncached is groupOf before memoization; it is asked once per type per
// group, which at 800 groups is 3.8 million calls over 4 800 types.
func (b *builder) groupOfUncached(typeName string) string {
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

// fieldInGroup is inGroup for one field. It differs only on a root type,
// whose fields are grouped one by one; see Config.RootFieldGroup.
func (b *builder) fieldInGroup(typeName string, fd *ast.FieldDefinition, group string) bool {
	if group == "" {
		return true
	}
	if !b.isRoot(typeName) {
		return b.groupOf(typeName) == group
	}
	return b.rootFieldGroup(typeName, fd) == group
}

func (b *builder) rootFieldGroup(root string, fd *ast.FieldDefinition) string {
	key := root + "." + fd.Name
	if g, ok := b.groupByRootField[key]; ok {
		return g
	}
	g := b.rootFieldGroupUncached(root, fd)
	if b.groupByRootField == nil {
		b.groupByRootField = map[string]string{}
	}
	b.groupByRootField[key] = g
	return g
}

func (b *builder) rootFieldGroupUncached(root string, fd *ast.FieldDefinition) string {
	file := ""
	if fd.Position != nil && fd.Position.Src != nil {
		file = fd.Position.Src.Name
	}
	if b.cfg.RootFieldGroup != nil {
		ret := fd.Type.Name()
		retGroup := ""
		if def := b.schema.Types[ret]; def != nil && !def.BuiltIn && !b.isRoot(ret) {
			retGroup = b.groupOf(ret)
		}
		if g := b.cfg.RootFieldGroup(RootField{
			Root: root, Name: fd.Name, SDLFile: file, ReturnType: ret, ReturnGroup: retGroup,
		}); g != "" {
			return sanitizeGroup(g, b.pkgName)
		}
	}
	if b.cfg.GroupFunc != nil {
		if g := b.cfg.GroupFunc(root, file); g != "" {
			return sanitizeGroup(g, b.pkgName)
		}
	}
	return sanitizeGroup(fileStem(file), b.pkgName)
}

// groupField is one object field and the type declaring it.
type groupField struct {
	typeName string
	fd       *ast.FieldDefinition
}

// eachGroupField calls fn for every field of every object type in group, in
// type then declaration order, skipping introspection fields.
//
// The fields are indexed by group once. Asking every type for every group was
// groups times types: at 800 groups it was a fifth of a generate, and the
// emitter asks three times per group.
func (b *builder) eachGroupField(group string, fn func(typeName string, fd *ast.FieldDefinition)) {
	if b.fieldsByGroup == nil {
		b.fieldScans++
		b.fieldsByGroup = map[string][]groupField{}
		for _, name := range b.typeNames(ast.Object) {
			for _, fd := range b.schema.Types[name].Fields {
				if strings.HasPrefix(fd.Name, "__") {
					continue
				}
				f := groupField{name, fd}
				b.fieldsByGroup[""] = append(b.fieldsByGroup[""], f)
				g := b.groupOf(name)
				if b.isRoot(name) {
					g = b.rootFieldGroup(name, fd)
				}
				b.fieldsByGroup[g] = append(b.fieldsByGroup[g], f)
			}
		}
	}
	for _, f := range b.fieldsByGroup[group] {
		fn(f.typeName, f.fd)
	}
}

// uniqueGroups lists the groups, once: the import block of every generated
// file asks for it through modelPkgNames.
func (b *builder) uniqueGroups() []string {
	if b.groups != nil {
		return b.groups
	}
	b.groupScans++
	seen := map[string]bool{}
	for _, kind := range []ast.DefinitionKind{ast.Object, ast.InputObject, ast.Enum, ast.Scalar, ast.Interface, ast.Union} {
		for _, name := range b.typeNames(kind) {
			if !b.groupEmits(name) {
				continue
			}
			// A root is not emitted as a type; its fields are, each in its own
			// group, so those are the groups it brings.
			if b.isRoot(name) {
				for _, fd := range b.schema.Types[name].Fields {
					if !strings.HasPrefix(fd.Name, "__") {
						seen[b.rootFieldGroup(name, fd)] = true
					}
				}
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
	b.groups = out
	return out
}

func (b *builder) groupEmits(typeName string) bool {
	def := b.schema.Types[typeName]
	if def == nil || def.BuiltIn {
		return false
	}
	if def.Kind == ast.Scalar {
		// A mapped scalar has no model, but one that encodes itself has a
		// binding, and its group has to exist to carry it.
		_, mapped := b.cfg.Models[typeName]
		return !mapped || b.marshalers[typeName]
	}
	return true
}

func (b *builder) hasResolver(group string) bool {
	found := false
	b.eachGroupField(group, func(name string, fd *ast.FieldDefinition) {
		if b.fieldKind(name, fd) == fieldResolve {
			found = true
		}
	})
	return found
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
	case "model", "internal",
		// A program, not an importable package.
		"main",
		// Names the generated code already imports under: a group package
		// called one of them redeclares it in every file that imports both.
		"graphql", "context", "embed":
		s += "grp"
	default:
		// func.graphql would otherwise ask for `package func`.
		if s == pkgName || token.IsKeyword(s) {
			s += "grp"
		}
	}
	return s
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
