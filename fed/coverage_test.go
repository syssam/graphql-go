package fed_test

import (
	"context"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

const coveredSubgraphSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT

type User @key(fields: "id") @requiresScopes(scopes: [["user:read"]]) {
  id: ID!
  name: String!
}

type Query {
  me: User! @requiresScopes(scopes: [["user:read"]])
  health: String! @public
}
`

type note struct{}

func coveredSubgraph(sdl string, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	src, bindings, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err != nil {
		return nil, err
	}
	return graphql.NewSchema(src, append([]graphql.SchemaOption{
		bindings,
		graphql.RequireAuthCoverage(),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		graphql.Query(
			graphql.Field("me", func(graphql.Root) *user { return users["1"] }),
			graphql.Field("health", func(graphql.Root) string { return "ok" }),
		),
	}, opts...)...)
}

// RequireAuthCoverage refuses a field that says neither who may read it nor
// that anyone may. The prelude adds three fields the author cannot say either
// about, because they are not in the author's text: without an answer of
// their own, coverage and federation could not be used together except by
// marking the whole Query type @public, which exempts every root field the
// author did write.
func TestAuthCoverageAcceptsTheProtocolFields(t *testing.T) {
	s, err := coveredSubgraph(coveredSubgraphSDL)
	if err != nil {
		t.Fatalf("a subgraph whose own fields are all covered was refused: %v", err)
	}

	// Exempt from coverage, which is all they are: what they return is not. An
	// entity fetched through _entities is still its own type's fields.
	e := graphql.NewExecutor(s, graphql.WithAuthorizer(graphql.ScopeAuthorizer(
		func(context.Context) map[string]bool { return nil })))
	resp := e.Execute(context.Background(), &graphql.Request{
		Query:     `query($r: [_Any!]!) { _entities(representations: $r) { ... on User { name } } }`,
		Variables: []byte(`{"r":[{"__typename":"User","id":"1"}]}`),
	})
	if strings.Contains(string(resp.Data), "Ada") {
		t.Fatalf("an entity's guarded field was served through _entities: %s", resp.Data)
	}
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "user:read") {
		t.Fatalf("errors = %v, want User.name refused for its own requirement", resp.Errors)
	}
}

// The exemption is the three protocol fields and nothing else. A field the
// author wrote and said nothing about is still refused, whichever type it is
// on -- Query included, which is the type the protocol fields are added to.
func TestAuthCoverageStillRefusesTheAuthorsOwnFields(t *testing.T) {
	for _, c := range []struct {
		name, extend, coord string
		bind                graphql.SchemaOption
	}{
		{"a root field", `extend type Query { open: String! }`, "Query.open",
			graphql.Query(graphql.Field("open", func(graphql.Root) string { return "" }))},
		// User's requirement is on the type, so a field added to it would be
		// covered; Note is a type of the author's that says nothing.
		{"a field of another type", `type Note { text: String! } extend type Query { note: Note! @public }`, "Note.text",
			graphql.Options(
				graphql.Object[note]("Note", graphql.Field("text", func(*note) string { return "" })),
				graphql.Query(graphql.Field("note", func(graphql.Root) *note { return &note{} })),
			)},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := coveredSubgraph(coveredSubgraphSDL+c.extend, c.bind)
			if err == nil {
				t.Fatal("a field declaring no authorization was accepted")
			}
			if !strings.Contains(err.Error(), c.coord+" declares no authorization") {
				t.Fatalf("error does not name %s: %v", c.coord, err)
			}
			for _, protocol := range []string{"Query._service", "Query._entities", "_Service.sdl"} {
				if strings.Contains(err.Error(), protocol) {
					t.Fatalf("%s was refused, and the author cannot annotate it: %v", protocol, err)
				}
			}
		})
	}
}

// A subgraph that declares no @public, or declares it for objects only, must
// build exactly as it did: the prelude writes no directive of the author's
// onto its own fields, and most subgraphs never ask for coverage at all.
func TestSubgraphWithoutAPublicDirectiveIsUnchanged(t *testing.T) {
	for _, c := range []struct{ name, sdl string }{
		{"not declared", subgraphSDL},
		{"declared for objects only", "directive @public on OBJECT\n" + subgraphSDL},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, bindings, err := fed.Subgraph(c.sdl,
				fed.Resolver("User", resolveUser), fed.Resolver("Product", resolveProduct))
			if err != nil {
				t.Fatal(err)
			}
			_, err = graphql.NewSchema(src, bindings,
				graphql.Object[user]("User",
					graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
					graphql.Field("name", func(u *user) string { return u.Name }),
				),
				graphql.Object[product]("Product",
					graphql.Field("sku", func(p *product) string { return p.SKU }),
					graphql.Field("price", func(p *product) int { return p.Price }),
				),
				graphql.Query(graphql.Field("me", func(graphql.Root) *user { return users["1"] })),
			)
			if err != nil {
				t.Fatalf("NewSchema: %v", err)
			}
		})
	}
}

// A requirement on the Query type reaches the protocol fields as it reaches
// every other root field: the exemption answers coverage and lifts nothing.
// That is how a subgraph keeps _entities to its router.
func TestAQueryTypeRequirementStillGuardsTheProtocolFields(t *testing.T) {
	sdl := strings.Replace(coveredSubgraphSDL, "type Query {",
		`type Query @requiresScopes(scopes: [["router"]]) {`, 1)
	s, err := coveredSubgraph(sdl)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s, graphql.WithAuthorizer(graphql.ScopeAuthorizer(
		func(context.Context) map[string]bool { return nil })))
	resp := e.Execute(context.Background(), &graphql.Request{Query: `{ _service { sdl } }`})
	if strings.Contains(string(resp.Data), "sdl") || len(resp.Errors) == 0 {
		t.Fatalf("_service was served without the Query type's requirement: %s %v", resp.Data, resp.Errors)
	}
}

// The protocol fields are exempt from coverage whatever the author's SDL
// declares. Marked @public instead, they were covered only in a subgraph that
// declared that directive for fields: one that guards every field with a
// requirement, and has no use for @public, could not ask for coverage at all,
// because the three fields it was refused for are not in its text to mark.
func TestAuthCoverageExemptsTheProtocolFieldsWithoutAPublicDirective(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT

type User @key(fields: "id") @requiresScopes(scopes: [["user:read"]]) {
  id: ID!
  name: String!
}

type Query {
  me: User! @requiresScopes(scopes: [["user:read"]])
}
`
	src, bindings, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err != nil {
		t.Fatal(err)
	}
	bind := []graphql.SchemaOption{
		bindings,
		graphql.RequireAuthCoverage(),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		graphql.Query(graphql.Field("me", func(graphql.Root) *user { return users["1"] })),
	}
	if _, err := graphql.NewSchema(src, bind...); err != nil {
		t.Fatalf("a subgraph whose own fields are all covered was refused: %v", err)
	}

	// And only those three: a field of the author's that says nothing is
	// still refused in the same subgraph.
	src, bindings, err = fed.Subgraph(sdl+`extend type Query { open: String! }`, fed.Resolver("User", resolveUser))
	if err != nil {
		t.Fatal(err)
	}
	bind[0] = bindings
	bind = append(bind, graphql.Query(graphql.Field("open", func(graphql.Root) string { return "" })))
	_, err = graphql.NewSchema(src, bind...)
	if err == nil || !strings.Contains(err.Error(), "Query.open declares no authorization") {
		t.Fatalf("err = %v, want Query.open refused", err)
	}
	if strings.Contains(err.Error(), "_service") || strings.Contains(err.Error(), "_entities") {
		t.Fatalf("a protocol field was refused: %v", err)
	}
}
