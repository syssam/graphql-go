package sdlprint

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
)

// everyConstruct declares each definition kind, each place a description or
// directive can sit, and each value kind a default can take, so that the
// comparison with the formatter reaches every branch of Print.
const everyConstruct = `"""Root schema"""
schema @meta { query: Q mutation: M subscription: S }
directive @meta on SCHEMA
"""Access"""
directive @auth("""who""" role: String! = "admin", level: Int) repeatable on FIELD_DEFINITION | OBJECT
directive @plain on ENUM_VALUE
"A timestamp"
scalar Time @specifiedBy(url: "https://example.com/time")
"""
Something with an id.
Second line.
"""
interface Node { id: ID! }
interface Named implements Node { id: ID! name: String }
type Q {
  "Look one up"
  node(id: ID!, "how deep" depth: Int = 2, flag: Boolean = true @deprecated(reason: "no")): Node
  users(filter: UserFilter = {role: ADMIN, tags: ["a", "b"], nested: {n: 1.5}}, first: Int = null): [User!]! @auth(role: "admin")
  s: Search
  noArgs: [[String]!]
}
type M { noop(a: Int, b: String): Boolean }
type S { tick: Int }
type User implements Node & Named @auth(role: "user", level: 2) {
  id: ID!
  name: String @deprecated(reason: "use fullName")
  old: String @deprecated
  fullName: String!
  at: Time
}
type Post implements Node { id: ID! }
union Search = User | Post
enum Role { ADMIN "The rest" USER @deprecated(reason: "gone") @plain }
input UserFilter { role: Role = USER, tags: [String!], nested: Inner, legacy: String @deprecated(reason: "x") }
input Inner { n: Float = 0.5 }
`

func TestPrintMatchesFormatter(t *testing.T) {
	corpus := map[string]string{
		"everyConstruct": everyConstruct,
		// Directives on a schema whose roots keep their default names take
		// the formatter's extend-schema branch instead of a definition.
		"extendSchema": "directive @m on SCHEMA\nschema @m { query: Query }\ntype Query { a: Int }\n",
	}
	for _, dir := range []string{"benchmarks", "examples/blog/graph/schema", "examples/blog", "examples/quickstart", "examples/relaynode", "examples/storefront"} {
		b, err := os.ReadFile(filepath.Join("..", "..", dir, "schema.graphql"))
		if err != nil {
			t.Fatal(err)
		}
		corpus[dir] = string(b)
	}
	for name, sdl := range corpus {
		t.Run(name, func(t *testing.T) {
			s := load(t, sdl)
			// The formatter drops the schema description, which Print keeps;
			// TestPrintKeepsDescriptions covers it, so compare without it.
			s.Description = ""
			var want bytes.Buffer
			formatter.NewFormatter(&want).FormatSchema(s)
			if got := Print(s); got != want.String() {
				t.Errorf("Print differs from the formatter\n--- got\n%s\n--- want\n%s", got, want.String())
			}
		})
	}
}

// A block string cannot hold every string: it drops leading and trailing blank
// lines and the indentation common to every line, normalizes line
// terminators, and """ ends it unless escaped.
var descriptions = []struct{ name, desc string }{
	{"plain", "plain text"},
	{"multi line", "line one\n  indented two"},
	{"triple quote", `say """ to quote`},
	{"escaped triple quote", `a \""" stays`},
	{"trailing quote", `ends with "`},
	{"trailing backslash", `ends with \`},
	{"leading space", "  leading spaces"},
	{"every line indented", "  one\n    two"},
	{"leading blank line", "\nafter a blank line"},
	{"leading whitespace line", "   \nafter a whitespace line"},
	{"trailing newline", "trailing newline\n"},
	{"inner whitespace line", "one\n   \ntwo"},
	{"carriage return", "one\rtwo"},
	{"tab", "a\tb"},
	{"astral non-printable", "tag \U000E0001 end"},
}

func TestPrintKeepsDescriptions(t *testing.T) {
	for _, tc := range descriptions {
		t.Run(tc.name, func(t *testing.T) {
			// strconv.Quote writes U+E0001 as \U000e0001, which GraphQL does
			// not accept; the source holds the rune itself instead.
			q := strings.ReplaceAll(strconv.Quote(tc.desc), `\U000e0001`, "\U000E0001")
			sdl := q + "\nschema { query: Query }\n" +
				q + "\ndirective @d(" + q + " x: Int) on FIELD_DEFINITION\n" +
				q + "\ntype Query {\n  " + q + "\n  f(" + q + " a: In): E\n}\n" +
				q + "\nenum E { " + q + " V }\n" +
				q + "\ninput In { " + q + " v: String }\n"
			again := reload(t, sdl)
			q2 := again.Types["Query"]
			for where, got := range map[string]string{
				"schema":        again.Description,
				"directive":     again.Directives["d"].Description,
				"directive arg": again.Directives["d"].Arguments.ForName("x").Description,
				"type":          q2.Description,
				"field":         q2.Fields.ForName("f").Description,
				"field arg":     q2.Fields.ForName("f").Arguments.ForName("a").Description,
				"enum":          again.Types["E"].Description,
				"enum value":    again.Types["E"].EnumValues.ForName("V").Description,
				"input":         again.Types["In"].Description,
				"input field":   again.Types["In"].Fields.ForName("v").Description,
			} {
				if got != tc.desc {
					t.Errorf("%s description = %q, want %q", where, got, tc.desc)
				}
			}
		})
	}
}

// With the default root names the formatter writes no schema definition, so
// a description must force one.
func TestPrintKeepsSchemaDescriptionWithDefaultRootNames(t *testing.T) {
	again := reload(t, "\"root\"\nschema { query: Query mutation: Mutation }\ntype Query { f: Int }\ntype Mutation { f: Int }\n")
	if again.Description != "root" {
		t.Errorf("schema description = %q, want %q", again.Description, "root")
	}
	if again.Mutation == nil {
		t.Error("mutation root lost")
	}
}

// ast.Value.String quotes with strconv.Quote, which emits \U and \x escapes
// that GraphQL does not accept; a default value or a directive argument
// holding such a character must still reload.
func TestPrintQuotesStringValues(t *testing.T) {
	const v = "tag \U000E0001 del \x7f quote \" slash \\ nl \n end"
	// The literal is spelled by hand: strconv.Quote would write the very \U
	// and \x escapes under test.
	const lit = `"tag ` + "\U000E0001" + ` del \u007F quote \" slash \\ nl \n end"`
	sdl := "directive @d(r: String) on FIELD_DEFINITION\ntype Query { f(a: String = " +
		lit + "): Int @d(r: " + lit + ") }\n"
	again := reload(t, sdl)
	f := again.Types["Query"].Fields.ForName("f")
	if got := f.Arguments.ForName("a").DefaultValue.Raw; got != v {
		t.Errorf("default value = %q, want %q", got, v)
	}
	if got := f.Directives.ForName("d").Arguments.ForName("r").Value.Raw; got != v {
		t.Errorf("directive argument = %q, want %q", got, v)
	}
}

func load(t *testing.T, sdl string) *ast.Schema {
	t.Helper()
	s, err := gqlparser.LoadSchema(&ast.Source{Name: "in.graphql", Input: sdl})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reload(t *testing.T, sdl string) *ast.Schema {
	t.Helper()
	printed := Print(load(t, sdl))
	again, err := gqlparser.LoadSchema(&ast.Source{Name: "out.graphql", Input: printed})
	if err != nil {
		t.Fatalf("printed SDL does not load: %v\n%s", err, printed)
	}
	return again
}
