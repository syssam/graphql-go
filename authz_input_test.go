package graphql

import (
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// authzInputSDL is named distinctly from input_test.go's inputSDL, which
// already exists in this package for the decode tests.
const authzInputSDL = `
enum OrderField { NAME TAX_NUMBER }
enum Direction { ASC DESC }
input Order { field: OrderField! direction: Direction = ASC }
input Where { not: Where and: [Where!] nameContains: String taxNumber: String }
input Patch { name: String taxNumber: String }
type Query { customers(where: Where, orderBy: [Order!], groupBy: [OrderField!]): [String!]! }
type Mutation { update(patch: Patch!): String }
`

// supplied parses a query and returns the argument's type, supplied value and
// the schema's types, so each case states the operation exactly as a client
// would send it.
func supplied(t *testing.T, query, field, arg string) (map[string]*ast.Definition, *ast.Type, *ast.Value) {
	t.Helper()
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: authzInputSDL})
	doc, errs := gqlparser.LoadQuery(schema, query)
	if errs != nil {
		t.Fatalf("query: %v", errs)
	}
	sel := doc.Operations[0].SelectionSet[0].(*ast.Field)
	if sel.Name != field {
		t.Fatalf("first selection is %s, want %s", sel.Name, field)
	}
	def := sel.Definition.Arguments.ForName(arg)
	var v *ast.Value
	if a := sel.Arguments.ForName(arg); a != nil {
		v = a.Value
	}
	return schema.Types, def.Type, v
}

func render(keys []InputKey) []string {
	var out []string
	for _, k := range keys {
		s := strings.Join(k.Path, ".")
		if k.Enum != "" {
			s += "=" + k.Enum
		}
		if k.Null {
			s += "=null"
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestInputKeys(t *testing.T) {
	cases := []struct {
		name  string
		query string
		field string
		arg   string
		vars  map[string]any
		want  []string
	}{
		{"filter keys, scalar values never reported",
			`{ customers(where: {nameContains: "ada", taxNumber: "123"}) }`, "customers", "where", nil,
			[]string{"nameContains", "taxNumber"}},
		{"nested and list, indices omitted, deduplicated",
			`{ customers(where: {and: [{taxNumber: "1"}, {taxNumber: "2"}], not: {nameContains: "x"}}) }`, "customers", "where", nil,
			[]string{"and", "and.taxNumber", "not", "not.nameContains"}},
		{"ordering by enum value",
			`{ customers(orderBy: [{field: TAX_NUMBER}]) }`, "customers", "orderBy", nil,
			[]string{"field=TAX_NUMBER"}},
		{"SDL input-field default is not reported",
			`{ customers(orderBy: [{field: NAME}]) }`, "customers", "orderBy", nil,
			[]string{"field=NAME"}},
		{"enum list argument reports each value",
			`{ customers(groupBy: [NAME, TAX_NUMBER]) }`, "customers", "groupBy", nil,
			[]string{"=NAME", "=TAX_NUMBER"}},
		{"variable object typed against the schema",
			`query($o: [Order!]) { customers(orderBy: $o) }`, "customers", "orderBy",
			map[string]any{"o": []any{map[string]any{"field": "TAX_NUMBER", "direction": "DESC"}}},
			[]string{"direction=DESC", "field=TAX_NUMBER"}},
		{"variable nested inside a literal",
			`query($f: OrderField!) { customers(orderBy: [{field: $f}]) }`, "customers", "orderBy",
			map[string]any{"f": "TAX_NUMBER"},
			[]string{"field=TAX_NUMBER"}},
		{"absent variable reports nothing",
			`query($w: Where) { customers(where: $w) }`, "customers", "where", map[string]any{},
			nil},
		{"explicit null key is reported",
			`mutation { update(patch: {taxNumber: null}) }`, "update", "patch", nil,
			[]string{"taxNumber=null"}},
		{"absent key is not a write",
			`mutation { update(patch: {name: "a"}) }`, "update", "patch", nil,
			[]string{"name"}},
		{"explicit null through a variable",
			`mutation($p: Patch!) { update(patch: $p) }`, "update", "patch",
			map[string]any{"p": map[string]any{"taxNumber": nil}},
			[]string{"taxNumber=null"}},
		{"argument not supplied",
			`{ customers }`, "customers", "where", nil,
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			types, typ, v := supplied(t, tc.query, tc.field, tc.arg)
			var got []InputKey
			if v != nil {
				got = inputKeys(types, typ, v, tc.vars)
			}
			if r := render(got); !slices.Equal(r, tc.want) {
				t.Errorf("keys = %v, want %v", r, tc.want)
			}
		})
	}
}

// A variable's value never appears in a key: only paths and enum identifiers.
func TestInputKeysNeverCarryScalarValues(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	keys := inputKeys(types, typ, v, map[string]any{"w": map[string]any{"taxNumber": "SECRET-123"}})
	for _, k := range keys {
		if strings.Contains(strings.Join(k.Path, "."), "SECRET") || strings.Contains(k.Enum, "SECRET") {
			t.Fatalf("a scalar value leaked into %+v", k)
		}
	}
}
