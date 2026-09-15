package gen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/syssam/graphql-go/codegen"
)

// writeGraphQLGo runs gqlc against the same schema, binding the same shared
// structs, then writes one Resolver implementing every group's interface.
func writeGraphQLGo(root string, n int) error {
	// gqlgen represents ID as string; map ours the same way so both engines
	// bind to identical struct fields.
	models := map[string]string{"Time": "time.Time", "ID": "string"}
	for _, name := range objectTypes(n) {
		models[name] = ModulePath + "/shared." + name
	}

	// Clear the tree first. Regenerating at a smaller entity count otherwise
	// leaves the previous run's SDL and packages behind, and the schema then
	// fails to build on types that no longer have bindings.
	if err := os.RemoveAll(filepath.Join(root, "graphqlgo", "gen")); err != nil {
		return err
	}

	if err := codegen.Generate(context.Background(), codegen.Config{
		Dir:         root,
		SchemaGlobs: []string{"schema/*.graphql"},
		Output:      "graphqlgo/gen",
		Package:     ModulePath + "/graphqlgo/gen",
		Models:      models,
	}); err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	return writeFile(filepath.Join(root, "graphqlgo", "gen", "resolvers.go"), graphqlGoResolvers(n))
}

func graphqlGoResolvers(n int) string {
	var b strings.Builder
	b.WriteString(header("gen"))

	imports := []string{`"context"`, "", `"` + ModulePath + `/shared"`}
	for i := range n {
		imports = append(imports, fmt.Sprintf(`"%s/graphqlgo/gen/%s"`, ModulePath, strings.ToLower(EntityName(i))))
	}
	imports = append(imports, `"`+ModulePath+`/graphqlgo/gen/prelude"`)
	fmt.Fprintf(&b, "import (\n\t%s\n)\n", indentJoin(imports))

	b.WriteString(`
// Resolver implements every group's Resolver interface over the shared
// dataset. Method names are unique across groups, so one type can serve them
// all.
type Resolver struct{}

// All returns the Resolvers value NewSchema expects.
func All() Resolvers {
	r := Resolver{}
	return Resolvers{
`)
	for i := range n {
		fmt.Fprintf(&b, "\t\t%s: r,\n", EntityName(i))
	}
	b.WriteString("\t\tPrelude: r,\n\t}\n}\n")

	b.WriteString(`
func firstOf(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func (Resolver) Ping(_ context.Context) (bool, error) { return true, nil }
`)

	for i := range n {
		name := EntityName(i)
		owner := EntityName((i + 1) % n)
		child := EntityName((i + 2) % n)
		pkg := strings.ToLower(name)

		fmt.Fprintf(&b, `
func (Resolver) %[1]sOwner(_ context.Context, obj *shared.%[1]s) (*shared.%[2]s, error) {
	return shared.Owner%[1]s(obj), nil
}

func (Resolver) %[1]sChildren(_ context.Context, obj *shared.%[1]s, args %[4]s.%[1]sChildrenArgs) (*shared.%[3]sConnection, error) {
	return shared.Children%[1]s(obj, firstOf(args.First)), nil
}

func (Resolver) %[1]sConnectionEdges(_ context.Context, obj *shared.%[1]sConnection) ([]*shared.%[1]sEdge, error) {
	return shared.Edges%[1]s(obj), nil
}

func (Resolver) %[1]sConnectionPageInfo(_ context.Context, obj *shared.%[1]sConnection) (*shared.PageInfo, error) {
	return shared.PageInfo%[1]s(obj), nil
}

func (Resolver) %[1]sEdgeNode(_ context.Context, obj *shared.%[1]sEdge) (*shared.%[1]s, error) {
	return obj.Node, nil
}

func (Resolver) %[1]s(_ context.Context, args prelude.%[1]sArgs) (*shared.%[1]s, error) {
	return shared.Get%[1]s(string(args.ID)), nil
}

func (Resolver) %[1]ss(_ context.Context, args prelude.%[1]ssArgs) (*shared.%[1]sConnection, error) {
	return shared.Conn%[1]s(firstOf(args.First)), nil
}

func (Resolver) Create%[1]s(_ context.Context, args prelude.Create%[1]sArgs) (*shared.%[1]s, error) {
	v := *shared.List%[1]s()[0]
	v.Name = args.Input.Name
	return &v, nil
}

func (Resolver) Update%[1]s(_ context.Context, args prelude.Update%[1]sArgs) (*shared.%[1]s, error) {
	src := shared.Get%[1]s(string(args.ID))
	if src == nil {
		return nil, nil
	}
	v := *src
	if args.Input.Name != nil {
		v.Name = *args.Input.Name
	}
	return &v, nil
}

func (Resolver) Delete%[1]s(_ context.Context, args prelude.Delete%[1]sArgs) (bool, error) {
	return shared.Get%[1]s(string(args.ID)) != nil, nil
}
`, name, owner, child, pkg)
	}

	fmt.Fprintf(&b, `
func (Resolver) Node(_ context.Context, args prelude.NodeArgs) (any, error) {
	if v := shared.Get%[1]s(string(args.ID)); v != nil {
		return v, nil
	}
	return nil, nil
}
`, EntityName(0))

	return b.String()
}
