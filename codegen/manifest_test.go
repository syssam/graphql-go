package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const manifestSDL = `
type Product {
  id: ID!
  title: String!
  slug: String!
  priceWithTax(rate: Float!): Float!
  stock: Int!
  related: [Product!]!
}
type Query { product(id: ID!): Product }
`

// generate runs the generator into a temp directory and returns it.
func generate(t *testing.T, sdl string, cfg Config) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Dir = dir
	cfg.SchemaGlobs = []string{"schema.graphql"}
	if cfg.Output == "" {
		cfg.Output = "graph"
	}
	if cfg.Package == "" {
		cfg.Package = "hello/graph"
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return dir
}

func generated(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "graph", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestManifestBindsAnExternalType is the case the mode exists for: an ORM
// already has the Go type, so no model is generated and the bindings point at
// its own package.
func TestManifestBindsAnExternalType(t *testing.T) {
	dir := generate(t, manifestSDL, Config{
		Manifest: &Manifest{Types: []TypeBinding{{
			Name: "Product",
			Go:   GoType{PkgPath: "example.com/shop/ent", Name: "Product"},
			Fields: map[string]FieldBinding{
				"id":    {Kind: FieldStruct},
				"title": {Kind: FieldStruct},
				"slug":  {Kind: FieldMethod},
				"stock": {Kind: FieldMethod, Context: true, Error: true, GoName: "StockLevel"},
			},
		}}},
	})

	src := generated(t, dir, "generated.go")
	for _, want := range []string{
		`graphql.Object[ent.Product]("Product"`,
		`graphql.Field("id", func(v *ent.Product) graphql.ID { return v.ID })`,
		`graphql.Field("slug", func(v *ent.Product) string { return v.Slug() })`,
		`graphql.Resolve("stock", func(ctx context.Context, v *ent.Product) (int, error) { return v.StockLevel(ctx) })`,
		`"example.com/shop/ent"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing:\n  %s\ngot:\n%s", want, src)
		}
	}

	// A bound type gets no generated model. With every type bound there is
	// nothing left to put in the model package, so it is not written at all
	// rather than written empty.
	switch models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go")); {
	case os.IsNotExist(err):
	case err != nil:
		t.Fatal(err)
	case strings.Contains(string(models), "type Product struct"):
		t.Errorf("a manifest-bound type should not get a generated model:\n%s", models)
	}
}

// TestManifestUnlistedFieldsAreResolvers pins the rule that keeps a partial
// manifest safe: an unlisted field becomes a resolver method, which can always
// be written, rather than generated access to a struct field that may not
// exist.
func TestManifestUnlistedFieldsAreResolvers(t *testing.T) {
	dir := generate(t, manifestSDL, Config{
		Manifest: &Manifest{Types: []TypeBinding{{
			Name:   "Product",
			Go:     GoType{PkgPath: "example.com/shop/ent", Name: "Product"},
			Fields: map[string]FieldBinding{"id": {Kind: FieldStruct}},
		}}},
	})
	src := generated(t, dir, "generated.go")

	// title is a plain scalar that inference would have made a struct field.
	if !strings.Contains(src, "Title(ctx context.Context, obj *ent.Product) (string, error)") {
		t.Errorf("unlisted title should be a resolver method:\n%s", src)
	}
	if strings.Contains(src, "return v.Title }") {
		t.Errorf("unlisted title should not be read from the struct:\n%s", src)
	}
}

func TestManifestMethodWithArguments(t *testing.T) {
	dir := generate(t, manifestSDL, Config{
		Manifest: &Manifest{Types: []TypeBinding{{
			Name: "Product",
			Fields: map[string]FieldBinding{
				"priceWithTax": {Kind: FieldMethod},
			},
		}}},
	})
	src := generated(t, dir, "generated.go")
	if !strings.Contains(src, `graphql.FieldArgs("priceWithTax", func(v *model.Product, a ProductPriceWithTaxArgs) float64 { return v.PriceWithTax(a.Rate) })`) {
		t.Errorf("method with arguments not emitted as expected:\n%s", src)
	}
}

func TestManifestGroupOverride(t *testing.T) {
	const sdl = `
type Product { id: ID! }
type Order { id: ID! }
type Query { product(id: ID!): Product order(id: ID!): Order }
`
	dir := generate(t, sdl, Config{
		Manifest: &Manifest{Types: []TypeBinding{
			{Name: "Product", Group: "catalog"},
			{Name: "Order", Group: "sales"},
		}},
	})
	for _, rel := range []string{"catalog/generated.go", "sales/generated.go"} {
		if _, err := os.Stat(filepath.Join(dir, "graph", filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name string
		man  *Manifest
		want string
	}{
		{
			"unknown type",
			&Manifest{Types: []TypeBinding{{Name: "Nope"}}},
			"type Nope is not defined in the schema",
		},
		{
			"unknown field",
			&Manifest{Types: []TypeBinding{{
				Name:   "Product",
				Fields: map[string]FieldBinding{"nope": {Kind: FieldStruct}},
			}}},
			"field Product.nope is not defined in the schema",
		},
		{
			"struct field with arguments",
			&Manifest{Types: []TypeBinding{{
				Name:   "Product",
				Fields: map[string]FieldBinding{"priceWithTax": {Kind: FieldStruct}},
			}}},
			"takes arguments and cannot be a struct field",
		},
		{
			"duplicate type",
			&Manifest{Types: []TypeBinding{{Name: "Product"}, {Name: "Product"}}},
			"type Product is bound more than once",
		},
		{
			"nameless binding",
			&Manifest{Types: []TypeBinding{{}}},
			"a type binding has no Name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(manifestSDL), 0o644); err != nil {
				t.Fatal(err)
			}
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"schema.graphql"},
				Output: "graph", Package: "hello/graph", Manifest: tc.man,
			})
			if err == nil {
				t.Fatal("wanted an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestManifestContradictsModels catches the configuration that would silently
// pick one of two answers.
func TestManifestContradictsModels(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(manifestSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		Models: map[string]string{"Product": "example.com/other.Product"},
		Manifest: &Manifest{Types: []TypeBinding{{
			Name: "Product",
			Go:   GoType{PkgPath: "example.com/shop/ent", Name: "Product"},
		}}},
	})
	if err == nil {
		t.Fatal("wanted an error")
	}
	if !strings.Contains(err.Error(), "but Models says") {
		t.Fatalf("error = %v", err)
	}
}

// TestGeneratedManifestExecutes compiles the generated package against a real
// Go type and runs a query through it, which is the only way to know the
// emitted field access actually type-checks.
func TestGeneratedManifestExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	const sdl = `
type Product {
  id: ID!
  title: String!
  slug: String!
  priceWithTax(rate: Float!): Float!
  stock: Int!
}
type Query { product(id: ID!): Product }
`
	dir := generate(t, sdl, Config{
		Manifest: &Manifest{Types: []TypeBinding{{
			Name: "Product",
			Go:   GoType{PkgPath: "hello/ent", Name: "Product"},
			Fields: map[string]FieldBinding{
				"id":           {Kind: FieldStruct},
				"title":        {Kind: FieldStruct},
				"slug":         {Kind: FieldMethod},
				"priceWithTax": {Kind: FieldMethod},
				"stock":        {Kind: FieldMethod, Context: true, Error: true, GoName: "StockLevel"},
			},
		}}},
	})
	writeTempModule(t, dir)

	ent := `package ent

import (
	"context"
	"strings"

	"github.com/syssam/graphql-go"
)

// Product stands in for an ORM-generated entity: some fields are struct data,
// some are methods, and none of them were written by the generator.
type Product struct {
	ID    graphql.ID
	Title string
	Price float64
}

func (p *Product) Slug() string { return strings.ToLower(strings.ReplaceAll(p.Title, " ", "-")) }

// The generator spreads arguments, so this takes plain values and the
// entity package never imports the generated one.
func (p *Product) PriceWithTax(rate float64) float64 { return p.Price * (1 + rate) }

func (p *Product) StockLevel(context.Context) (int, error) { return 7, nil }
`
	if err := os.MkdirAll(filepath.Join(dir, "ent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ent", "product.go"), []byte(ent), 0o644); err != nil {
		t.Fatal(err)
	}

	stub := `package graph

import (
	"context"

	"hello/ent"
)

type Stub struct{}

func (Stub) Product(_ context.Context, a ProductArgs) (*ent.Product, error) {
	return &ent.Product{ID: a.ID, Title: "Thin Mints", Price: 10}, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}

	testSrc := `package graph_test

import (
	"context"
	"testing"

	"github.com/syssam/graphql-go"
	"hello/graph"
)

func TestManifestBoundQuery(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: ` + "`" + `{ product(id: "1") { id title slug priceWithTax(rate: 0.2) stock } }` + "`" + `,
	})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := ` + "`" + `{"product":{"id":"1","title":"Thin Mints","slug":"thin-mints","priceWithTax":12,"stock":7}}` + "`" + `
	if got := string(resp.Data); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "exec_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}
