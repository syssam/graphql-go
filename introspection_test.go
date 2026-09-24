package graphql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

const introSDL = `
"""The schema description."""
schema { query: Query }

"""A URL."""
scalar URL @specifiedBy(url: "https://example.com/url")

directive @tag(name: String! = "x", weight: Int @deprecated(reason: "unused")) repeatable on FIELD_DEFINITION | OBJECT

interface Node { id: ID! }
interface Named implements Node { id: ID! name: String! }

type Thing implements Node & Named {
  id: ID!
  """The name."""
  name(upper: Boolean = false, legacy: Int @deprecated): String!
  old: String @deprecated(reason: "use name")
  older: String @deprecated
  matrix: [[Int!]]!
}

enum Color { RED GREEN @deprecated(reason: "no green") }

input Where { id: ID, legacy: String @deprecated(reason: "gone"), color: Color = RED, nested: [Int!] = [1, 2], obj: Pick = {a: 1} }

input Pick @oneOf { a: Int, b: String }

type Query { thing(where: Where): Thing }
`

type iThing struct{ ID string }

func newIntrospectionExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(introSDL),
		Object[iThing]("Thing",
			Field("id", func(x *iThing) string { return x.ID }),
			Field("name", func(x *iThing) string { return x.ID }),
			Field("old", func(*iThing) *string { return nil }),
			Field("older", func(*iThing) *string { return nil }),
			Field("matrix", func(*iThing) [][]int { return [][]int{{1, 2}, {3}} }),
		),
		Object[Root]("Query", Resolve("thing", func(context.Context, Root) (*iThing, error) { return &iThing{ID: "t"}, nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// query runs an introspection query and decodes data into a generic map.
func introQuery(t *testing.T, e *Executor, q string) map[string]any {
	t.Helper()
	resp := run(t, e, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatalf("invalid JSON %s: %v", resp.Data, err)
	}
	return out
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func names(list any) string {
	items, _ := list.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, get(it, "name").(string))
	}
	return strings.Join(out, ",")
}

const graphiqlIntrospection = `
query IntrospectionQuery {
  __schema {
    description
    queryType { name } mutationType { name } subscriptionType { name }
    types { ...FullType }
    directives { name description isRepeatable locations args(includeDeprecated: true) { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description specifiedByURL isOneOf
  fields(includeDeprecated: true) { name description args(includeDeprecated: true) { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason }
  inputFields(includeDeprecated: true) { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue isDeprecated deprecationReason }
fragment TypeRef on __Type {
  kind name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }
}`

func TestIntrospectionFullQuery(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, graphiqlIntrospection)
	schema := get(data, "__schema")
	if get(schema, "description") != "The schema description." {
		t.Fatalf("description = %v", get(schema, "description"))
	}
	if get(schema, "queryType", "name") != "Query" || get(schema, "mutationType") != nil || get(schema, "subscriptionType") != nil {
		t.Fatalf("root types = %v %v %v", get(schema, "queryType"), get(schema, "mutationType"), get(schema, "subscriptionType"))
	}
	typeNames := names(get(schema, "types"))
	for _, want := range []string{"Boolean", "Color", "ID", "Named", "Node", "Pick", "Query", "String", "Thing", "URL", "Where", "__Schema", "__Type", "__TypeKind"} {
		if !strings.Contains(","+typeNames+",", ","+want+",") {
			t.Errorf("types lack %s: %s", want, typeNames)
		}
	}
	if !strings.HasPrefix(typeNames, "Boolean,Color,Float,ID,Int,Named,Node,Pick,Query,String,Thing,URL,Where,__") {
		t.Errorf("types must be sorted by name: %s", typeNames)
	}
	dirNames := names(get(schema, "directives"))
	for _, want := range []string{"deprecated", "include", "skip", "specifiedBy", "oneOf", "tag"} {
		if !strings.Contains(","+dirNames+",", ","+want+",") {
			t.Errorf("directives lack %s: %s", want, dirNames)
		}
	}

	// The fixture schema exercises interfaces, unions and every root type.
	_, fe := newFixtureExecutor(t)
	fdata := introQuery(t, fe, graphiqlIntrospection)
	if get(fdata, "__schema", "mutationType", "name") != "Mutation" {
		t.Fatalf("fixture mutation type = %v", get(fdata, "__schema", "mutationType"))
	}
}

func TestIntrospectionTypeRefs(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{ __type(name: "Thing") { kind name interfaces { name } fields { name type { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }`)
	thing := get(data, "__type")
	if get(thing, "kind") != "OBJECT" || names(get(thing, "interfaces")) != "Node,Named" {
		t.Fatalf("thing = %v", thing)
	}
	fields, _ := get(thing, "fields").([]any)
	if names(fields) != "id,name,matrix" {
		t.Fatalf("fields = %s (deprecated fields must be hidden by default)", names(fields))
	}
	matrix := fields[2]
	// [[Int!]]! → NON_NULL → LIST → LIST → NON_NULL → Int.
	if get(matrix, "type", "kind") != "NON_NULL" || get(matrix, "type", "ofType", "kind") != "LIST" ||
		get(matrix, "type", "ofType", "ofType", "kind") != "LIST" || get(matrix, "type", "ofType", "ofType", "ofType", "kind") != "NON_NULL" ||
		get(matrix, "type", "ofType", "ofType", "ofType", "name") != nil {
		out, _ := json.Marshal(matrix)
		t.Fatalf("matrix type chain = %s", out)
	}
	if get(fields[0], "type", "kind") != "NON_NULL" || get(fields[0], "type", "ofType", "name") != "ID" || get(fields[0], "type", "ofType", "kind") != "SCALAR" {
		t.Fatalf("id type = %v", get(fields[0], "type"))
	}

	data = introQuery(t, e, `{ __type(name: "Nope") { name } }`)
	if get(data, "__type") != nil {
		t.Fatalf("unknown type must be null, got %v", get(data, "__type"))
	}
}

func TestIntrospectionDeprecation(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{
		__type(name: "Thing") {
			fields(includeDeprecated: true) {
				name isDeprecated deprecationReason
				args { name } allArgs: args(includeDeprecated: true) { name isDeprecated deprecationReason defaultValue }
			}
		}
		color: __type(name: "Color") { enumValues { name } all: enumValues(includeDeprecated: true) { name isDeprecated deprecationReason } }
		where: __type(name: "Where") { kind isOneOf inputFields { name defaultValue type { kind name ofType { name } } } all: inputFields(includeDeprecated: true) { name isDeprecated deprecationReason } }
		pick: __type(name: "Pick") { isOneOf }
	}`)
	fields, _ := get(data, "__type", "fields").([]any)
	if names(fields) != "id,name,old,older,matrix" {
		t.Fatalf("fields = %s", names(fields))
	}
	if get(fields[2], "isDeprecated") != true || get(fields[2], "deprecationReason") != "use name" {
		t.Fatalf("old = %v", fields[2])
	}
	if get(fields[3], "isDeprecated") != true || get(fields[3], "deprecationReason") != "No longer supported" {
		t.Fatalf("older = %v", fields[3])
	}
	if get(fields[0], "isDeprecated") != false || get(fields[0], "deprecationReason") != nil {
		t.Fatalf("id = %v", fields[0])
	}
	if names(get(fields[1], "args")) != "upper" || names(get(fields[1], "allArgs")) != "upper,legacy" {
		t.Fatalf("name args = %s / %s", names(get(fields[1], "args")), names(get(fields[1], "allArgs")))
	}
	allArgs, _ := get(fields[1], "allArgs").([]any)
	if get(allArgs[0], "defaultValue") != "false" || get(allArgs[1], "defaultValue") != nil || get(allArgs[1], "isDeprecated") != true {
		t.Fatalf("name args detail = %v", allArgs)
	}

	if names(get(data, "color", "enumValues")) != "RED" || names(get(data, "color", "all")) != "RED,GREEN" {
		t.Fatalf("enum values = %v", get(data, "color"))
	}
	all, _ := get(data, "color", "all").([]any)
	if get(all[1], "deprecationReason") != "no green" {
		t.Fatalf("GREEN = %v", all[1])
	}

	where := get(data, "where")
	if get(where, "kind") != "INPUT_OBJECT" || get(where, "isOneOf") != false || get(data, "pick", "isOneOf") != true {
		t.Fatalf("where/pick = %v %v", where, get(data, "pick"))
	}
	inputs, _ := get(where, "inputFields").([]any)
	if names(inputs) != "id,color,nested,obj" || names(get(where, "all")) != "id,legacy,color,nested,obj" {
		t.Fatalf("input fields = %s / %s", names(inputs), names(get(where, "all")))
	}
	if get(inputs[0], "defaultValue") != nil || get(inputs[1], "defaultValue") != "RED" || get(inputs[2], "defaultValue") != "[1,2]" || get(inputs[3], "defaultValue") != "{a:1}" {
		out, _ := json.Marshal(inputs)
		t.Fatalf("default values = %s", out)
	}
	if get(inputs[2], "type", "kind") != "LIST" || get(inputs[2], "type", "ofType", "name") != nil {
		t.Fatalf("nested type = %v", get(inputs[2], "type"))
	}
}

func TestIntrospectionScalarsDirectivesAndAbstracts(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{
		url: __type(name: "URL") { kind description specifiedByURL fields { name } isOneOf }
		int: __type(name: "Int") { specifiedByURL }
		node: __type(name: "Node") { kind possibleTypes { name } interfaces { name } fields { name } }
		named: __type(name: "Named") { interfaces { name } possibleTypes { name } }
		__schema { directives { name isRepeatable locations args { name defaultValue } } }
	}`)
	url := get(data, "url")
	if get(url, "kind") != "SCALAR" || get(url, "description") != "A URL." || get(url, "specifiedByURL") != "https://example.com/url" || get(url, "fields") != nil || get(url, "isOneOf") != nil {
		t.Fatalf("URL = %v", url)
	}
	if get(data, "int", "specifiedByURL") != nil {
		t.Fatalf("Int specifiedByURL = %v", get(data, "int", "specifiedByURL"))
	}
	node := get(data, "node")
	if get(node, "kind") != "INTERFACE" || names(get(node, "possibleTypes")) != "Thing" || names(get(node, "interfaces")) != "" || names(get(node, "fields")) != "id" {
		t.Fatalf("Node = %v", node)
	}
	if names(get(data, "named", "interfaces")) != "Node" || names(get(data, "named", "possibleTypes")) != "Thing" {
		t.Fatalf("Named = %v", get(data, "named"))
	}
	var tag map[string]any
	for _, d := range get(data, "__schema", "directives").([]any) {
		if get(d, "name") == "tag" {
			tag = d.(map[string]any)
		}
	}
	if tag == nil || tag["isRepeatable"] != true {
		t.Fatalf("tag = %v", tag)
	}
	locs, _ := tag["locations"].([]any)
	if len(locs) != 2 || locs[0] != "FIELD_DEFINITION" || locs[1] != "OBJECT" {
		t.Fatalf("locations = %v", locs)
	}
	if names(tag["args"]) != "name" || get(tag["args"].([]any)[0], "defaultValue") != `"x"` {
		t.Fatalf("tag args = %v", tag["args"])
	}

	_, fe := newFixtureExecutor(t)
	fdata := introQuery(t, fe, `{ __type(name: "SearchResult") { kind possibleTypes { name } fields { name } } }`)
	if get(fdata, "__type", "kind") != "UNION" || names(get(fdata, "__type", "possibleTypes")) != "User,Post" || get(fdata, "__type", "fields") != nil {
		t.Fatalf("SearchResult = %v", get(fdata, "__type"))
	}
}

func TestIntrospectionTypenameOnMetaTypes(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{ __schema { __typename queryType { __typename fields { __typename } } } }`)
	if get(data, "__schema", "__typename") != "__Schema" || get(data, "__schema", "queryType", "__typename") != "__Type" {
		t.Fatalf("got %v", data)
	}
	resp := run(t, e, `{ thing { matrix } }`, "")
	expectData(t, resp, `{"thing":{"matrix":[[1,2],[3]]}}`)
}

// TestIntrospectionDirectiveDeprecation covers the introspection half of the
// draft's directives-on-directive-definitions change: __Directive carries a
// deprecation state, __Schema.directives filters on it, and
// DIRECTIVE_DEFINITION is a valid __DirectiveLocation value.
func TestIntrospectionDirectiveDeprecation(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{
	  __schema { directives(includeDeprecated: true) { name isDeprecated deprecationReason } }
	  __type(name: "__DirectiveLocation") { enumValues { name } }
	}`)

	dirs, _ := get(data, "__schema", "directives").([]any)
	if len(dirs) == 0 {
		t.Fatal("no directives returned")
	}
	for _, d := range dirs {
		name := get(d, "name")
		if got := get(d, "isDeprecated"); got != false {
			t.Errorf("directive %v: isDeprecated = %v, want false", name, got)
		}
		if got := get(d, "deprecationReason"); got != nil {
			t.Errorf("directive %v: deprecationReason = %v, want null", name, got)
		}
	}

	locs := names(get(data, "__type", "enumValues"))
	if !strings.Contains(","+locs+",", ",DIRECTIVE_DEFINITION,") {
		t.Errorf("__DirectiveLocation lacks DIRECTIVE_DEFINITION: %s", locs)
	}
}

// TestIntrospectionDirectivesDefaultsToVisible keeps the no-argument form of
// __Schema.directives working once the argument exists.
func TestIntrospectionDirectivesDefaultsToVisible(t *testing.T) {
	e := newIntrospectionExecutor(t)
	data := introQuery(t, e, `{__schema{directives{name}}}`)
	if got := names(get(data, "__schema", "directives")); !strings.Contains(got, "tag") {
		t.Errorf("directives = %s, want it to include tag", got)
	}
}

// A defaultValue is a GraphQL literal a client parses. gqlparser's
// ast.Value.String quotes with strconv.Quote, which writes \U and \x escapes
// GraphQL does not define, so a default holding such a character reached the
// client as a literal no GraphQL parser accepts.
func TestIntrospectionDefaultValueIsAGraphQLLiteral(t *testing.T) {
	const v = "tag \U000E0001 del \x7f end"
	s, err := NewSchema(SDL(`type Query { f(a: String = "tag `+"\U000E0001"+` del \u007F end"): Int }`),
		Args[struct{ A *string }](),
		Query(FieldArgs("f", func(Root, struct{ A *string }) int { return 0 })),
	)
	if err != nil {
		t.Fatal(err)
	}
	data := introQuery(t, NewExecutor(s), `{ __type(name: "Query") { fields { args { defaultValue } } } }`)
	lit, _ := get(data, "__type", "fields").([]any)[0].(map[string]any)["args"].([]any)[0].(map[string]any)["defaultValue"].(string)
	doc, gerr := parser.ParseQuery(&ast.Source{Input: "{ f(a: " + lit + ") }"})
	if gerr != nil {
		t.Fatalf("defaultValue %s does not parse as a GraphQL literal: %v", lit, gerr)
	}
	if got := doc.Operations[0].SelectionSet[0].(*ast.Field).Arguments[0].Value.Raw; got != v {
		t.Fatalf("defaultValue %s reads back as %q, want %q", lit, got, v)
	}
}
