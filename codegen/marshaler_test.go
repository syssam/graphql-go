package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// entgql and velox generate an order field as a struct holding a cursor func,
// with MarshalGQL and UnmarshalGQL, and an order input holding it; and an edge
// method taking that order. Until the engine could bind a type that encodes
// itself, the field was dropped, the order input with it, and every edge
// method taking an order fell to a hand-written resolver converting a
// generated string enum back. Now all three bind as they are, and so does a
// Cursor scalar with the same contract.
func TestAutoBindBindsTypesThatEncodeThemselves(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("schema.graphql", `directive @goModel(model: String) on OBJECT | INPUT_OBJECT | SCALAR | ENUM
scalar Cursor @goModel(model: "hello/ent.Cursor")
enum ProductOrderField @goModel(model: "hello/ent.ProductOrderField") { NAME PRICE }
input ProductOrder @goModel(model: "hello/ent.ProductOrder") { field: ProductOrderField! }
type Product @goModel(model: "hello/ent.Product") { name: String! }
type Category @goModel(model: "hello/ent.Category") {
  name: String!
  products(after: Cursor, orderBy: ProductOrder): [Product!]!
}
type Query { categories: [Category!]! }
`)
	write("ent/ent.go", `package ent

import (
	"context"
	"fmt"
	"io"
	"strconv"
)

type Cursor struct{ ID int }

func (c Cursor) MarshalGQL(w io.Writer) { io.WriteString(w, strconv.Quote(strconv.Itoa(c.ID))) }
func (c *Cursor) UnmarshalGQL(v any) error {
	s, _ := v.(string)
	n, err := strconv.Atoi(s)
	c.ID = n
	return err
}

type ProductOrderField struct {
	column   string
	toCursor func(*Product) Cursor
}

var byName = &ProductOrderField{column: "name"}

func (f ProductOrderField) MarshalGQL(w io.Writer) { io.WriteString(w, strconv.Quote("NAME")) }
func (f *ProductOrderField) UnmarshalGQL(v any) error {
	if v != "NAME" && v != "PRICE" {
		return fmt.Errorf("%v is not a ProductOrderField", v)
	}
	*f = *byName
	return nil
}

type ProductOrder struct {
	Field *ProductOrderField `+"`json:\"field\"`"+`
}

type Product struct{ Name string }

type Category struct{ Name string }

func (c *Category) Products(ctx context.Context, after *Cursor, orderBy *ProductOrder) ([]*Product, error) {
	return nil, nil
}
`)
	writeTempModule(t, dir)

	var notes []string
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		AutoBind:       []string{"./ent"},
		Notef:          func(f string, a ...any) { notes = append(notes, f) },
	}); err != nil {
		t.Fatal(err)
	}
	src := readFile(t, filepath.Join(dir, "graph", "generated.go"))
	for _, want := range []string{
		`graphql.EnumMarshaler[ent.ProductOrderField]("ProductOrderField")`,
		`graphql.ScalarMarshaler[ent.Cursor]("Cursor")`,
		`graphql.Input[ent.ProductOrder]("ProductOrder")`,
		`v.Products(ctx, a.After, a.OrderBy)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go lacks %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "CategoryProducts(") {
		t.Errorf("Category.products fell to the Resolver though the edge method takes the bound types\n%s", src)
	}
	for _, n := range notes {
		if strings.Contains(n, "dropped") || strings.Contains(n, "custom scalar") {
			t.Errorf("reported as unbound or dropped: %s", n)
		}
	}

	write("graph/validate_test.go", `package graph

import "testing"

func TestValidate(t *testing.T) {
	if err := ValidateSchema(); err != nil {
		t.Fatal(err)
	}
}
`)
	goBuild(t, dir)
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ValidateSchema: %v\n%s", err, out)
	}
}
