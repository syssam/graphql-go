package codegen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Auto-bind is only meaningfully testable against real Go types, because the
// whole feature is reading them. These tests write a package, load it and
// check what was discovered; the last one compiles and runs the result.

const autoSDL = `
type Product {
  id: ID!
  title: String!
  slug: String!
  priceWithTax(rate: Float!): Float!
  stock: Int!
  ownerID: ID!
  secret: String!
}
type Query { product(id: ID!): Product }
`

// entSource is a stand-in for an ORM-generated entity: struct fields, a
// promoted field from an embedded type, a json tag that does not match the Go
// name, plain and failing methods, and a method the generator must refuse.
const entSource = `package ent

import "context"

type Audit struct {
	OwnerID string ` + "`json:\"ownerID\"`" + `
}

type Product struct {
	Audit
	ID    string
	Title string
	Price float64
}

func (p *Product) Slug() string { return "slug-" + p.Title }

func (p *Product) PriceWithTax(rate float64) float64 { return p.Price * (1 + rate) }

func (p *Product) Stock(ctx context.Context) (int, error) { return 7, nil }

// Secret has a shape the generator cannot call, so the field must fall
// through to the resolver rather than generating something that will not
// compile.
func (p *Product) Secret(a, b, c int) (string, error, error) { return "", nil, nil }
`

// writeEntModule writes a module whose ent package auto-bind can load.
func writeEntModule(t *testing.T, sdl, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "ent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ent", "product.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func autoGenerate(t *testing.T, dir string, cfg Config) string {
	t.Helper()
	cfg.Dir = dir
	cfg.SchemaGlobs = []string{"schema.graphql"}
	cfg.Output = "graph"
	cfg.Package = "hello/graph"
	if cfg.AutoBind == nil {
		cfg.AutoBind = []string{"./ent"}
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAutoBindDiscoversFieldsAndMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	src := autoGenerate(t, writeEntModule(t, autoSDL, entSource), Config{})

	for _, want := range []string{
		// Bound to the discovered type, not to a generated model.
		`graphql.Object[ent.Product]("Product"`,
		// Plain struct fields, matched case-insensitively. id is stored as a
		// string, which an ID! field needs converting to: refusing that would
		// send the commonest field in any schema through a resolver.
		`graphql.Field("id", func(v *ent.Product) graphql.ID { return graphql.ID(v.ID) })`,
		// Matching types need no conversion.
		`graphql.Field("title", func(v *ent.Product) string { return v.Title })`,
		// Promoted from the embedded Audit, matched by its json tag rather
		// than by name: OwnerID would otherwise derive to ownerId.
		`graphql.Field("ownerID", func(v *ent.Product) graphql.ID { return graphql.ID(v.OwnerID) })`,
		// A plain method is pure and binds with Field.
		`graphql.Field("slug", func(v *ent.Product) string { return v.Slug() })`,
		// Arguments are spread, as in manifest mode.
		`graphql.FieldArgs("priceWithTax", func(v *ent.Product, a ProductPriceWithTaxArgs) float64 { return v.PriceWithTax(a.Rate) })`,
		// Context and error make it a resolver-scheduled call on the model.
		`graphql.Resolve("stock", func(ctx context.Context, v *ent.Product) (int, error) { return v.Stock(ctx) })`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing:\n  %s\ngot:\n%s", want, src)
		}
	}
}

// TestAutoBindRefusesUncallableMethods is the safety rule: anything not
// confidently matched falls through to the resolver, because a resolver method
// can always be written where a bad guess is a compile error in generated code.
func TestAutoBindRefusesUncallableMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	src := autoGenerate(t, writeEntModule(t, autoSDL, entSource), Config{})

	if strings.Contains(src, "v.Secret(") {
		t.Errorf("a method returning two errors should not have been bound:\n%s", src)
	}
	if !strings.Contains(src, "ProductSecret(ctx context.Context, obj *ent.Product) (string, error)") {
		t.Errorf("secret should have fallen through to the Resolver interface:\n%s", src)
	}
}

// TestAutoBindLeavesUnknownTypesAlone checks that discovery is additive: a
// schema type with no Go counterpart still gets a generated model.
func TestAutoBindLeavesUnknownTypesAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
type Product { id: ID! }
type Review { id: ID! body: String! }
type Query { product(id: ID!): Product }
`
	dir := writeEntModule(t, sdl, entSource)
	autoGenerate(t, dir, Config{})

	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(models), "type Review struct") {
		t.Errorf("Review has no Go counterpart and should still get a model:\n%s", models)
	}
	if strings.Contains(string(models), "type Product struct") {
		t.Errorf("Product was discovered and should not get a model:\n%s", models)
	}
}

// TestManifestOverridesDiscovery is the precedence rule: discovery is a guess
// from names, an explicit binding is a statement.
func TestManifestOverridesDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	src := autoGenerate(t, writeEntModule(t, autoSDL, entSource), Config{
		Manifest: &Manifest{Types: []TypeBinding{{
			Name:   "Product",
			Fields: map[string]FieldBinding{"title": {Kind: FieldResolver}},
		}}},
	})

	if strings.Contains(src, `graphql.Field("title", func(v *ent.Product) string { return v.Title })`) {
		t.Errorf("an explicit Resolver binding should beat the discovered struct field:\n%s", src)
	}
	if !strings.Contains(src, "ProductTitle(ctx context.Context, obj *ent.Product) (string, error)") {
		t.Errorf("title should be on the Resolver interface:\n%s", src)
	}
	// Fields the manifest did not mention keep their discovered bindings.
	if !strings.Contains(src, `graphql.Field("id", func(v *ent.Product) graphql.ID { return graphql.ID(v.ID) })`) {
		t.Errorf("unmentioned fields should keep discovery:\n%s", src)
	}
}

func TestAutoBindReportsLoadErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeEntModule(t, autoSDL, "package ent\n\nfunc broken() { this is not go }\n")
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph", AutoBind: []string{"./ent"},
	})
	if err == nil {
		t.Fatal("wanted an error from a package that does not compile")
	}
	if !strings.Contains(err.Error(), "auto-bind") {
		t.Fatalf("error should say where it came from: %v", err)
	}
}

// TestGeneratedAutoBindExecutes compiles what discovery produced and runs a
// query through it. Reading the emitted text cannot tell whether the field
// access and method calls actually type-check.
func TestGeneratedAutoBindExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := writeEntModule(t, autoSDL, entSource)
	autoGenerate(t, dir, Config{})

	stub := `package graph

import (
	"context"

	"github.com/syssam/graphql-go"
	"hello/ent"
)

type Stub struct{}

func (Stub) Product(_ context.Context, a ProductArgs) (*ent.Product, error) {
	p := &ent.Product{ID: string(a.ID), Title: "Thin Mints", Price: 10}
	p.OwnerID = "u1"
	return p, nil
}

func (Stub) ProductSecret(context.Context, *ent.Product) (string, error) { return "hidden", nil }

var _ = graphql.ID("")
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

func TestAutoBoundQuery(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: ` + "`" + `{ product(id: "1") { id title slug priceWithTax(rate: 0.2) stock ownerID secret } }` + "`" + `,
	})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := ` + "`" + `{"product":{"id":"1","title":"Thin Mints","slug":"slug-Thin Mints","priceWithTax":12,"stock":7,"ownerID":"u1","secret":"hidden"}}` + "`" + `
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

// A Go value that is always present satisfies a nullable SDL field: the value
// simply is never null, and the engine accepts the binding. AutoBind used to
// refuse it, so an ORM column that is NOT NULL under a nullable SDL field went
// to the Resolver -- 2 179 fields on one real schema, 29% of everything
// AutoBind left behind on types it had otherwise bound.
func TestAutoBindValueSatisfiesANullableField(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
type Reading {
  id: ID!
  celsius: Float
  label: String
  missing: Float
}
type Query { reading(id: ID!): Reading }
`
	const source = `package ent

type Reading struct {
	ID      string
	Celsius float64
	Label   string
}
`
	dir := writeEntModule(t, sdl, source)
	out := autoGenerate(t, dir, Config{})

	// Bound, returning the value type rather than a pointer to it.
	for _, want := range []string{
		`graphql.Field("celsius", func(v *ent.Reading) float64 { return v.Celsius })`,
		`graphql.Field("label", func(v *ent.Reading) string { return v.Label })`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated code is missing:\n  %s", want)
		}
	}
	// A field the Go type does not have is still a resolver.
	if !strings.Contains(out, "Missing(ctx context.Context, obj *ent.Reading) (*float64, error)") {
		t.Error("a field with no Go counterpart did not fall through to the Resolver")
	}

	// And it compiles. Reading the output is not evidence: this repository has
	// shipped generated code that read correctly and did not build, twice.
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if b, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, b)
	}
	build := exec.Command("go", "build", "./graph")
	build.Dir = dir
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, b)
	}
}

// A type the config binds by name is read wherever it lives. AutoBind names
// the packages to discover types in; a Models entry or a model directive has
// already said which type, and its package was loaded only for an enum or an
// input. So an object bound to a service's own struct had its fields guessed
// from the SDL: url became v.Url against a field called URL, which is a
// compile error in generated code, and a field the struct does not have was
// one too where it should have been a Resolver method.
func TestAutoBindReadsADeclaredObjectTypeOutsideItsPatterns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
type McpServer {
  slug: String!
  url: String!
  hasToken: Boolean!
  tools: Int!
  missing: String!
}
type Product { id: ID! title: String! }
type Query { server: McpServer product: Product }
`
	dir := writeEntModule(t, sdl, entSource)
	writeFiles(t, dir, map[string]string{
		"svc/svc.go": "package svc\n\ntype Server struct {\n\tSlug     string `json:\"slug\"`\n\tURL      string `json:\"url\"`\n\tHasToken bool\n}\n\nfunc (s *Server) Tools() int { return 3 }\n\n" +
			// Found by name in ./ent too; the declared one is the one read.
			"type Product struct {\n\tID    string\n\tLabel string `json:\"title\"`\n}\n",
	})
	src := autoGenerate(t, dir, Config{Models: map[string]string{
		"McpServer": "hello/svc.Server",
		"Product":   "hello/svc.Product",
	}})
	for _, want := range []string{
		`graphql.Object[svc.Server]("McpServer"`,
		`graphql.Field("slug", func(v *svc.Server) string { return v.Slug })`,
		`graphql.Field("url", func(v *svc.Server) string { return v.URL })`,
		`graphql.Field("hasToken", func(v *svc.Server) bool { return v.HasToken })`,
		`graphql.Field("tools", func(v *svc.Server) int { return v.Tools() })`,
		`McpServerMissing(`,
		`graphql.Field("title", func(v *svc.Product) string { return v.Label })`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing:\n  %s\ngot:\n%s", want, src)
		}
	}
	for _, guess := range []string{"v.Url", "v.Missing", "v.Title"} {
		if strings.Contains(src, guess) {
			t.Errorf("generated.go guesses %s, which the declared type does not have", guess)
		}
	}
	goBuild(t, dir)
}

// A struct written by hand holds the types this generator writes: an enum's
// model, another object's. Its package compiles only once those exist, so the
// first generate cannot read it, and says so; the second can. A field of a
// generated type then has to be recognised as answering the schema's, under
// whatever name the generated file imports that model package by. Read by
// package name alone, a field the struct already had became a Resolver method.
func TestAutoBindReadsAHandWrittenStructHoldingGeneratedTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
scalar Stamp
enum Colour { RED GREEN }
type Swatch { name: String! colour: Colour! shades: [Shade!]! made: Stamp }
type Shade { level: Int! }
type Product { id: ID! title: String! }
type Query { swatch: Swatch product: Product }
`
	dir := writeEntModule(t, sdl, entSource)
	writeFiles(t, dir, map[string]string{
		"svc/svc.go": "package svc\n\nimport \"hello/graph/model\"\n\ntype Swatch struct {\n\tName   string\n\tColour model.Colour\n\tShades []*model.Shade\n}\n",
		// A mapped package called model too, so the generated one is
		// imported as modelmodel and is not what its package clause says.
		"ext/model/model.go": "package model\n\ntype Stamp string\n",
	})
	var notes []string
	cfg := Config{
		Models: map[string]string{"Swatch": "hello/svc.Swatch", "Stamp": "hello/ext/model.Stamp"},
		Notef: func(format string, args ...any) {
			if note := fmt.Sprintf(format, args...); strings.Contains(note, "could not be read") {
				notes = append(notes, note)
			}
		},
	}
	autoGenerate(t, dir, cfg)
	if len(notes) != 1 || !strings.Contains(notes[0], "Swatch") {
		t.Errorf("notes = %q, want the first generate to say Swatch's type could not be read", notes)
	}
	notes = nil
	second := autoGenerate(t, dir, cfg)
	if len(notes) != 0 {
		t.Errorf("notes = %q, want none once svc builds", notes)
	}
	third := autoGenerate(t, dir, cfg)
	for _, want := range []string{
		`graphql.Field("name", func(v *svc.Swatch) string { return v.Name })`,
		`graphql.Field("colour", func(v *svc.Swatch) modelmodel.Colour { return v.Colour })`,
		`func(v *svc.Swatch) []*modelmodel.Shade { return v.Shades }`,
	} {
		if !strings.Contains(second, want) {
			t.Errorf("generated.go missing:\n  %s\ngot:\n%s", want, second)
		}
	}
	if strings.Contains(second, "SwatchColour(") || strings.Contains(second, "SwatchShades(") {
		t.Errorf("a field the struct holds became a Resolver method:\n%s", second)
	}
	if second != third {
		t.Errorf("two generates over one tree disagree:\n--- second\n%s\n--- third\n%s", second, third)
	}
	goBuild(t, dir)
}

// With more than one group the generated models are a package per group, and
// that is decided after discovery has compared every field. A hand-written
// struct holding another group's enum was measured against a type spelled
// model.Colour while its own field said paint.Colour, so on a real schema four
// fields the structs held fell to Resolver methods nobody had written.
func TestAutoBindRecognisesAnotherGroupsModelInAHandWrittenStruct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := t.TempDir()
	writeTempModule(t, dir)
	writeFiles(t, dir, map[string]string{
		"schema/paint.graphql":  "enum Colour { RED GREEN }\ntype Shade { level: Int! }\nextend type Query { shade: Shade }\n",
		"schema/swatch.graphql": "type Swatch { name: String! colour: Colour! shades: [Shade!]! }\nextend type Query { swatch: Swatch }\n",
		"schema/schema.graphql": "type Query { ping: Boolean }\n",
		"ent/ent.go":            "package ent\n",
		"svc/svc.go":            "package svc\n\nimport \"hello/graph/model/paint\"\n\ntype Swatch struct {\n\tName   string\n\tColour paint.Colour\n\tShades []*paint.Shade\n}\n",
	})
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		AutoBind: []string{"./ent"}, Models: map[string]string{"Swatch": "hello/svc.Swatch"},
	}
	// Once to write the models svc is built from, once to read svc.
	for range 2 {
		if err := Generate(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	src := readFile(t, filepath.Join(dir, "graph", "swatch", "generated.go"))
	for _, want := range []string{
		`graphql.Field("colour", func(v *svc.Swatch) paint.Colour { return v.Colour })`,
		`func(v *svc.Swatch) []*paint.Shade { return v.Shades }`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("swatch/generated.go missing:\n  %s\ngot:\n%s", want, src)
		}
	}
	if strings.Contains(src, "SwatchColour(") || strings.Contains(src, "SwatchShades(") {
		t.Errorf("a field the struct holds became a Resolver method:\n%s", src)
	}
	goBuild(t, dir)
}
