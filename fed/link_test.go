package fed_test

import (
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

// linkedSubgraph builds a subgraph whose only entity is User, under whatever
// spelling of the federation directives sdl uses.
func linkedSubgraph(t *testing.T, sdl string) *graphql.Executor {
	t.Helper()
	src, bindings, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "hi" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

func expectAda(t *testing.T, e *graphql.Executor) {
	t.Helper()
	resp := entities(t, e, `[{"__typename":"User","id":"1"}]`, `... on User { id name }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"_entities":[{"id":"1","name":"Ada"}]}` {
		t.Fatalf("data = %s", got)
	}
}

// Federation 2 imports nothing by default: a subgraph that links the
// specification without an import list writes every directive under the
// link's namespace, and that is as much a @key as the imported spelling.
func TestANamespacedKeyIsAKey(t *testing.T) {
	expectAda(t, linkedSubgraph(t, `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3")

type User @federation__key(fields: "id") @federation__shareable {
  id: ID!
  name: String! @federation__tag(name: "public")
}

type Query { hello: String! }
`))
}

// The link's own name is the namespace, so as: renames it.
func TestALinkAliasRenamesTheNamespace(t *testing.T) {
	expectAda(t, linkedSubgraph(t, `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", as: "fed")

type User @fed__key(fields: "id") { id: ID! name: String! }

type Query { hello: String! }
`))
}

// An import can rename one directive, which is how a schema that already has
// a @key of its own takes federation's.
func TestARenamedKeyImportIsAKey(t *testing.T) {
	expectAda(t, linkedSubgraph(t, `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", import: [{name: "@key", as: "@entity"}, "@shareable", {name: "@tag", as: "@label"}])

type User @entity(fields: "id") @shareable {
  id: ID!
  name: String! @label(name: "public")
}

type Query { hello: String! }
`))
}

// resolvable: false means the same under any spelling: the type is another
// subgraph's entity and needs no resolver here.
func TestAnUnresolvableKeyUnderAnotherNameNeedsNoResolver(t *testing.T) {
	const sdl = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3")

type User @federation__key(fields: "id") { id: ID! name: String! }
type Product @federation__key(fields: "sku", resolvable: false) { sku: String! }

type Query { hello: String! }
`
	if _, _, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser)); err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
}

// A renamed key is checked like any other: the name changes what the
// directive is called, not what it must select.
func TestARenamedKeyIsStillChecked(t *testing.T) {
	const sdl = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3")

type User @federation__key(fields: "nope") { id: ID! name: String! }

type Query { hello: String! }
`
	_, _, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want one naming nope", err)
	}
}

// Only the federation specification's link names federation directives. The
// same import under another specification's URL is that specification's.
func TestAnotherSpecificationsImportIsNotAKey(t *testing.T) {
	const sdl = `
extend schema @link(url: "https://specs.example.com/other/v1.0", import: [{name: "@key", as: "@entity"}])

type User @entity(fields: "id") { id: ID! name: String! }

type Query { hello: String! }
`
	_, _, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err == nil || !strings.Contains(err.Error(), "no resolvable @key") {
		t.Fatalf("err = %v, want User refused as not an entity", err)
	}
}
