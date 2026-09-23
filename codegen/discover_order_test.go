package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Discovery decides a field by comparing the Go type it found against the Go
// type that field needs, and the second half of that comparison is only right
// once every type this pass binds is in the model map. These tests pin the
// ordering; each of them passes trivially if the binding is merely absent, so
// each asserts the binding is present rather than that nothing broke.

func TestDiscoveryBindsObjectValuedFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
type Owner { id: ID! }
type Product {
  id: ID!
  owner: Owner!
  vendor: Owner
  reseller: Owner!
}
type Query { product(id: ID!): Product }
`
	const source = `package ent

type Owner struct{ ID string }

type Product struct {
	ID     string
	Owner  *Owner
	Vendor *Owner
}

func (p *Product) Reseller() *Owner { return p.Owner }
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})
	for _, want := range []string{
		`graphql.Field("owner", func(v *ent.Product) *ent.Owner { return v.Owner })`,
		`graphql.Field("vendor", func(v *ent.Product) *ent.Owner { return v.Vendor })`,
		`graphql.Field("reseller", func(v *ent.Product) *ent.Owner { return v.Reseller() })`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("not bound: %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "ProductOwner(") {
		t.Error("an object-valued struct field fell through to the resolver")
	}
}

// A binding the SDL carries has to reach the builder's own model map before
// discovery reads it. foldModelDirective returns a new map, so it did not, and
// every field typed by a directive-bound type was measured against the model
// the generator would have written instead.
func TestDirectiveBindingReachesDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on OBJECT
type Owner @goModel(model: "hello/other.Owner") { id: ID! }
type Product { id: ID! owner: Owner! }
type Query { product(id: ID!): Product }
`
	const source = `package ent

import "hello/other"

type Product struct {
	ID    string
	Owner *other.Owner
}
`
	dir := writeEntModule(t, sdl, source)
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other", "owner.go"),
		[]byte("package other\n\ntype Owner struct{ ID string }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := autoGenerate(t, dir, Config{ModelDirective: ModelDirective{Name: "goModel", Arg: "model"}})
	if !strings.Contains(src, `graphql.Field("owner", func(v *ent.Product) *other.Owner { return v.Owner })`) {
		t.Errorf("a field typed by a directive-bound type did not bind\n%s", src)
	}
}

// The bindings above are only right if the engine accepts them, and a struct
// field holding a pointer to another bound model is the shape most likely to
// be subtly wrong.
func TestDiscoveredObjectFieldExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	const sdl = `
type Owner { id: ID! name: String! }
type Product { id: ID! owner: Owner! vendor: Owner }
type Query { product(id: ID!): Product }
`
	const source = `package ent

type Owner struct {
	ID   string
	Name string
}

type Product struct {
	ID     string
	Owner  *Owner
	Vendor *Owner
}
`
	dir := writeEntModule(t, sdl, source)
	autoGenerate(t, dir, Config{})

	stub := `package graph

import (
	"context"

	"hello/ent"
)

type Stub struct{}

func (Stub) Product(_ context.Context, a ProductArgs) (*ent.Product, error) {
	return &ent.Product{ID: string(a.ID), Owner: &ent.Owner{ID: "u1", Name: "Ada"}}, nil
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

func TestDiscoveredObjectField(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: ` + "`" + `{ product(id: "1") { id owner { id name } vendor { id } } }` + "`" + `,
	})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := ` + "`" + `{"product":{"id":"1","owner":{"id":"u1","name":"Ada"},"vendor":null}}` + "`" + `
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

// Fields must be read off the Go type the schema will use, not off whichever
// type discovery found under that name. Two packages each holding a
// PaymentTerms -- one with a HasEarlyPaymentDiscount method and one without --
// bound the field to a method the declared type does not have, which is a
// compile error in a file marked DO NOT EDIT.
func TestFieldsComeFromTheDeclaredType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	const sdl = `
directive @goModel(model: String) on OBJECT
type PaymentTerms @goModel(model: "hello/entity.PaymentTerms") {
  id: ID!
  hasEarlyPaymentDiscount: Boolean!
}
type Query { terms(id: ID!): PaymentTerms }
`
	// ./ent is the auto-bound package and holds the richer PaymentTerms; the
	// declaration points at entity, which does not have the method.
	const source = `package ent

type PaymentTerms struct{ ID string }

func (p *PaymentTerms) HasEarlyPaymentDiscount() bool { return true }
`
	dir := writeEntModule(t, sdl, source)
	if err := os.MkdirAll(filepath.Join(dir, "entity"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entity", "terms.go"),
		[]byte("package entity\n\ntype PaymentTerms struct{ ID string }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		AutoBind:       []string{"./ent", "./entity"},
	})
	if strings.Contains(src, "v.HasEarlyPaymentDiscount()") {
		t.Errorf("a method of the type that was not declared was bound\n%s", src)
	}
	if !strings.Contains(src, "PaymentTermsHasEarlyPaymentDiscount(ctx context.Context, obj *entity.PaymentTerms)") {
		t.Errorf("the field did not fall through to the resolver\n%s", src)
	}
	// The id field is on both, so it still binds: this is not "declared means
	// discover nothing".
	if !strings.Contains(src, `graphql.Field("id", func(v *entity.PaymentTerms) graphql.ID { return graphql.ID(v.ID) })`) {
		t.Errorf("a field the declared type does have was not bound\n%s", src)
	}
}

// A method binds on its result type, and until this test nothing checked its
// parameters -- fits() compared the count and stopped. The generator spreads
// the args struct's fields straight into the call, so every parameter has to
// be exactly the Go type that argument's field holds. An ORM edge method
// taking its own *ent.XOrder while the SDL's XOrder is modelled by the
// generator compiles to `cannot use a.OrderBy (*model.XOrder) as *ent.XOrder`,
// in a file marked DO NOT EDIT.
func TestAMethodMustMatchItsArgumentTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
input Order { dir: String! }
type Doc {
  id: ID!
  children(order: Order): [Doc!]!
  head(n: Int!): String!
}
type Query { doc(id: ID!): Doc }
`
	// Order is not mapped, so the generator models it and the argument field
	// is *model.Order -- the method's own *ent.Order cannot answer it. head
	// is the control: its parameter really is the type the argument holds.
	const source = `package ent

type Order struct{ Dir string }

type Doc struct{ ID string }

func (d *Doc) Children(order *Order) []*Doc { return nil }

func (d *Doc) Head(n int) string { return d.ID }
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})

	if strings.Contains(src, "v.Children(") {
		t.Errorf("children bound to a method whose parameter type does not match\n%s", src)
	}
	if !strings.Contains(src, "DocChildren(ctx context.Context, obj *ent.Doc, args DocChildrenArgs)") {
		t.Errorf("children did not fall through to the resolver\n%s", src)
	}
	// The control: a method whose parameters do match still binds, so this is
	// a type check and not "methods with arguments are never bound".
	if !strings.Contains(src, "v.Head(a.N)") {
		t.Errorf("a method whose parameters do match stopped binding\n%s", src)
	}
}
