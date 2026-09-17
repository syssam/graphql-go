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
//
// JSON is a custom scalar that accepts a bare identifier literal (fix
// round 1, R3): the same lexical shape as an enum value, so it is the fixture
// for proving a custom scalar is never mistaken for one. Where.and takes
// nullable elements so a literal or variable list can carry an explicit null
// element (R5a) without failing validation. Choice is a oneOf input (item 4);
// gqlparser enforces its "exactly one key" rule itself, and the walk needs no
// special case for it because it never looks at directives.
const authzInputSDL = `
scalar JSON

directive @oneOf on INPUT_OBJECT

enum OrderField { NAME TAX_NUMBER }
enum Direction { ASC DESC }
input Order { field: OrderField! direction: Direction = ASC }
input Where { not: Where and: [Where] nameContains: String taxNumber: String meta: JSON metaTags: [JSON] }
input Patch { name: String taxNumber: String }
input Choice @oneOf { byId: ID byName: String }
type Query {
  customers(where: Where, orderBy: [Order!], groupBy: [OrderField!], meta: JSON, tags: [String]): [String!]!
  lookup(choice: Choice): String
}
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

		// Fix round 1, R3: a custom scalar's bare-identifier literal is
		// lexically an ast.EnumValue but must never be reported as Enum.
		{"custom scalar literal at top level is not an enum",
			`{ customers(meta: SECRET) }`, "customers", "meta", nil,
			nil},
		{"custom scalar literal in an input field is a bare key",
			`{ customers(where: {meta: SECRET}) }`, "customers", "where", nil,
			[]string{"meta"}},
		{"custom scalar literal in a list is a bare key, deduplicated",
			`{ customers(where: {metaTags: [SECRET1, SECRET2]}) }`, "customers", "where", nil,
			[]string{"metaTags"}},

		// Fix round 1, R4: a literal key whose value is a variable resolving
		// to an object or list is itself reported, exactly as for a literal
		// composite value at that key.
		{"literal key holding an empty variable object is still reported",
			`query($w: Where) { customers(where: {not: $w}) }`, "customers", "where",
			map[string]any{"w": map[string]any{}},
			[]string{"not"}},
		{"literal key holding an empty variable list is still reported",
			`query($a: [Where]) { customers(where: {and: $a}) }`, "customers", "where",
			map[string]any{"a": []any{}},
			[]string{"and"}},
		{"literal key holding a non-empty variable object reports the key and its contents",
			`query($w: Where) { customers(where: {not: $w}) }`, "customers", "where",
			map[string]any{"w": map[string]any{"nameContains": "x"}},
			[]string{"not", "not.nameContains"}},

		// Fix round 1, R5a: a null list element is not a null at the list's
		// key; the key is still reported as an intermediate key when nested.
		{"null element in a nested literal list is not a null at that key",
			`{ customers(where: {and: [null]}) }`, "customers", "where", nil,
			[]string{"and"}},
		{"null element in a top-level literal list reports nothing",
			`{ customers(tags: [null]) }`, "customers", "tags", nil,
			nil},
		{"null element in a nested variable list is not a null at that key",
			`query($a: [Where]) { customers(where: {and: $a}) }`, "customers", "where",
			map[string]any{"a": []any{nil}},
			[]string{"and"}},
		{"null element in a top-level variable list reports nothing",
			`query($t: [String]) { customers(tags: $t) }`, "customers", "tags",
			map[string]any{"t": []any{nil}},
			nil},

		// Fix round 1, R5c: an enum value a variable supplies that is not
		// declared on the enum is not reported at all; coercion would have
		// rejected it.
		{"an enum value not declared on the enum is not reported",
			`query($f: OrderField!) { customers(orderBy: [{field: $f}]) }`, "customers", "orderBy",
			map[string]any{"f": "BOGUS"},
			nil},

		// Item 4 follow-ups.
		{"argument itself sent as an explicit null",
			`{ customers(where: null) }`, "customers", "where", nil,
			[]string{"=null"}},
		{"argument sent as a variable resolved to null",
			`query($w: Where) { customers(where: $w) }`, "customers", "where",
			map[string]any{"w": nil},
			[]string{"=null"}},
		{"a single value stands in for a list through a variable",
			`query($o: [Order!]) { customers(orderBy: $o) }`, "customers", "orderBy",
			map[string]any{"o": map[string]any{"field": "TAX_NUMBER", "direction": "DESC"}},
			[]string{"direction=DESC", "field=TAX_NUMBER"}},
		{"a oneOf input reports whichever field was supplied",
			`{ lookup(choice: {byId: "1"}) }`, "lookup", "choice", nil,
			[]string{"byId"}},
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

// Fix round 1, R5b: the variable-object walk must visit map keys in sorted
// order so the reported slice is deterministic. This does not go through
// render(), which sorts its output and so cannot tell a fixed order from a
// lucky one; asserting the raw slice order is the point.
func TestInputKeysVariableMapOrderDeterministic(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	vars := map[string]any{"w": map[string]any{
		"taxNumber":    nil,
		"and":          nil,
		"not":          nil,
		"nameContains": nil,
	}}
	keys := inputKeys(types, typ, v, vars)
	var got []string
	for _, k := range keys {
		got = append(got, strings.Join(k.Path, "."))
	}
	want := []string{"and", "nameContains", "not", "taxNumber"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v (map iteration must be sorted)", got, want)
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

const inputDirectiveSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION | FIELD_DEFINITION | INPUT_FIELD_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
enum OrderField { NAME TAX_NUMBER }
input Where { nameContains: String }
`

func TestAuthorizeInputPlacementAtBuild(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr string // empty means the schema must build
	}{
		{"input object argument", `type Query { c(where: Where @authorizeInput(kind: FILTER)): String }`, ""},
		{"enum list argument", `type Query { c(groupBy: [OrderField!] @authorizeInput(kind: FILTER)): String }`, ""},
		{"scalar argument", `type Query { c(q: String @authorizeInput(kind: FILTER)): String }`, "Query.c(q:)"},
		{"on a field", `type Query { c: String @authorizeInput(kind: FILTER) }`, "Query.c"},
		{"on an input field", `input Bad { a: Where @authorizeInput(kind: FILTER) } type Query { c(b: Bad): String }`, "Bad.a"},
		{"on an interface field argument", `interface Node { c(where: Where @authorizeInput(kind: FILTER)): String } type Query { c: String }`, "Node.c(where:)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(inputDirectiveSDL+tc.extra), Query(Field("c", func(Root) *string { return nil })))
			if tc.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "authorizeInput") {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "authorizeInput") {
				t.Fatalf("want an @authorizeInput error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
}
