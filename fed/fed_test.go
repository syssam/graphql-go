package fed_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

type user struct {
	ID   string
	Name string
}
type product struct {
	SKU   string
	Price int
}

const subgraphSDL = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", import: ["@key", "@shareable"])

type User @key(fields: "id") {
  id: ID!
  name: String!
}

type Product @key(fields: "sku") @shareable {
  sku: String!
  price: Int!
}

type Query { me: User! }
`

var users = map[string]*user{"1": {ID: "1", Name: "Ada"}}
var products = map[string]*product{"abc": {SKU: "abc", Price: 42}}

func resolveUser(_ context.Context, r fed.Representation) (*user, error) {
	id, _ := r["id"].(string)
	return users[id], nil
}

func resolveProduct(_ context.Context, r fed.Representation) (*product, error) {
	sku, _ := r["sku"].(string)
	return products[sku], nil
}

func newSubgraph(t *testing.T, entities ...fed.Entity) *graphql.Executor {
	t.Helper()
	if len(entities) == 0 {
		entities = []fed.Entity{
			fed.Resolver("User", resolveUser),
			fed.Resolver("Product", resolveProduct),
		}
	}
	src, bindings, err := fed.Subgraph(subgraphSDL, entities...)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		graphql.Object[product]("Product",
			graphql.Field("sku", func(p *product) string { return p.SKU }),
			graphql.Field("price", func(p *product) int { return p.Price }),
		),
		graphql.Query(graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) {
			return users["1"], nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

func entities(t *testing.T, e *graphql.Executor, reps string, sel string) *graphql.Response {
	t.Helper()
	return e.Execute(context.Background(), &graphql.Request{
		Query:     `query($r: [_Any!]!) { _entities(representations: $r) { ` + sel + ` } }`,
		Variables: json.RawMessage(`{"r":` + reps + `}`),
	})
}

// The router composes from what _service returns, so it must be the author's
// SDL as written, federation directives and all.
func TestServiceReturnsTheAuthorsSDL(t *testing.T) {
	resp := newSubgraph(t).Execute(context.Background(), &graphql.Request{Query: `{ _service { sdl } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	var out struct {
		Service struct{ SDL string } `json:"_service"`
	}
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Service.SDL != subgraphSDL {
		t.Fatalf("sdl is not the author's text:\n%s", out.Service.SDL)
	}
}

func TestEntitiesResolvesEachRepresentation(t *testing.T) {
	resp := entities(t, newSubgraph(t),
		`[{"__typename":"User","id":"1"},{"__typename":"Product","sku":"abc"}]`,
		`... on User { id name } ... on Product { sku price }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"_entities":[{"id":"1","name":"Ada"},{"sku":"abc","price":42}]}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

// An entity the subgraph does not own, and one it owns but cannot find, are
// both null rather than errors: _Entity elements are nullable for this.
func TestEntitiesReturnsNullForUnknownAndMissing(t *testing.T) {
	resp := entities(t, newSubgraph(t),
		`[{"__typename":"Nope","id":"1"},{"__typename":"User","id":"404"}]`,
		`... on User { id }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"_entities":[null,null]}` {
		t.Fatalf("data = %s", got)
	}
}

func TestEntityResolverErrorFailsTheField(t *testing.T) {
	e := newSubgraph(t,
		fed.Resolver("User", func(context.Context, fed.Representation) (*user, error) {
			return nil, errors.New("datastore unavailable")
		}),
		fed.Resolver("Product", resolveProduct),
	)
	resp := entities(t, e, `[{"__typename":"User","id":"1"}]`, `... on User { id }`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "datastore unavailable") {
		t.Fatalf("errors = %v", resp.Errors)
	}
}

// A @key type with no resolver answers null to every router fetch for it,
// which is a start-up mistake and knowable at start-up.
func TestSubgraphRejectsAKeyTypeWithNoResolver(t *testing.T) {
	_, _, err := fed.Subgraph(subgraphSDL, fed.Resolver("User", resolveUser))
	if err == nil || !strings.Contains(err.Error(), "Product") {
		t.Fatalf("err = %v, want one naming Product", err)
	}
}

func TestSubgraphRejectsAResolverForANonEntity(t *testing.T) {
	_, _, err := fed.Subgraph(subgraphSDL,
		fed.Resolver("User", resolveUser),
		fed.Resolver("Product", resolveProduct),
		fed.Resolver("Nope", resolveUser),
	)
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("err = %v, want one naming Nope", err)
	}
}

// A subgraph with no entities must not declare _Entity or _entities at all;
// the composition rules say an empty union is invalid.
func TestSubgraphWithNoEntitiesOmitsTheEntityMachinery(t *testing.T) {
	const plain = `type Query { hello: String! }`
	src, bindings, err := fed.Subgraph(plain)
	if err != nil {
		t.Fatal(err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Query(graphql.Resolve("hello", func(context.Context, graphql.Root) (string, error) {
			return "hi", nil
		})))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := graphql.NewExecutor(s)
	if resp := e.Execute(context.Background(), &graphql.Request{Query: `{ _service { sdl } }`}); len(resp.Errors) > 0 {
		t.Fatalf("_service should still exist: %v", resp.Errors)
	}
	resp := e.Execute(context.Background(), &graphql.Request{Query: `{ _entities(representations: []) { __typename } }`})
	if len(resp.Errors) == 0 {
		t.Fatal("_entities must not exist on a subgraph with no entities")
	}
}

// @key is allowed on an interface, but a union's members must be object
// types, so an entity interface cannot go into _Entity. Apollo's rule is the
// same: only object types are members.
func TestEntityInterfaceIsNotAUnionMember(t *testing.T) {
	const sdl = `
interface Media @key(fields: "id") { id: ID! }
type Movie implements Media @key(fields: "id") { id: ID! title: String! }
type Query { hello: String! }
`
	type movie struct {
		ID    string
		Title string
	}
	src, bindings, err := fed.Subgraph(sdl,
		fed.Resolver("Movie", func(context.Context, fed.Representation) (*movie, error) { return nil, nil }),
	)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	_, err = graphql.NewSchema(src, bindings,
		graphql.Interface[any]("Media"),
		graphql.Object[movie]("Movie",
			graphql.Field("id", func(m *movie) graphql.ID { return graphql.ID(m.ID) }),
			graphql.Field("title", func(m *movie) string { return m.Title }),
		),
		graphql.Query(graphql.Resolve("hello", func(context.Context, graphql.Root) (string, error) {
			return "hi", nil
		})))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}

// A resolver for an entity interface is a mistake with a specific cause, so
// the error names it rather than saying the type carries no @key.
func TestSubgraphRejectsAResolverForAnEntityInterface(t *testing.T) {
	const sdl = `
interface Media @key(fields: "id") { id: ID! }
type Movie implements Media @key(fields: "id") { id: ID! }
type Query { hello: String! }
`
	type movie struct{ ID string }
	_, _, err := fed.Subgraph(sdl,
		fed.Resolver("Movie", func(context.Context, fed.Representation) (*movie, error) { return nil, nil }),
		fed.Resolver("Media", func(context.Context, fed.Representation) (*movie, error) { return nil, nil }),
	)
	if err == nil || !strings.Contains(err.Error(), "__typename") {
		t.Fatalf("err = %v, want one explaining the concrete __typename", err)
	}
}

// A @key naming a field the type does not have composes into a router that
// fetches nothing. The SDL is in hand at start-up, so say so then.
func TestSubgraphRejectsAKeyFieldThatDoesNotExist(t *testing.T) {
	const sdl = `
type User @key(fields: "nope") { id: ID! }
type Query { hello: String! }
`
	type u struct{ ID string }
	_, _, err := fed.Subgraph(sdl, fed.Resolver("User", func(context.Context, fed.Representation) (*u, error) {
		return nil, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "User") {
		t.Fatalf("err = %v, want one naming User and nope", err)
	}
}

// A compound key selects into another type, and that nesting is checked too.
func TestSubgraphAcceptsANestedKey(t *testing.T) {
	const sdl = `
type Org { id: ID! }
type User @key(fields: "id org { id }") { id: ID! org: Org! }
type Query { hello: String! }
`
	type u struct{ ID string }
	if _, _, err := fed.Subgraph(sdl, fed.Resolver("User", func(context.Context, fed.Representation) (*u, error) {
		return nil, nil
	})); err != nil {
		t.Fatalf("a valid nested key was rejected: %v", err)
	}
}

func TestSubgraphRejectsANestedKeyFieldThatDoesNotExist(t *testing.T) {
	const sdl = `
type Org { id: ID! }
type User @key(fields: "org { nope }") { id: ID! org: Org! }
type Query { hello: String! }
`
	type u struct{ ID string }
	_, _, err := fed.Subgraph(sdl, fed.Resolver("User", func(context.Context, fed.Representation) (*u, error) {
		return nil, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "Org") {
		t.Fatalf("err = %v, want one naming Org and nope", err)
	}
}

func TestSubgraphRejectsAMalformedKey(t *testing.T) {
	const sdl = `
type User @key(fields: "id {") { id: ID! }
type Query { hello: String! }
`
	type u struct{ ID string }
	if _, _, err := fed.Subgraph(sdl, fed.Resolver("User", func(context.Context, fed.Representation) (*u, error) {
		return nil, nil
	})); err == nil {
		t.Fatal("a field set that does not parse was accepted")
	}
}

// The pattern beyond a plain key lookup: a field this subgraph owns computed
// from one it does not, declared @external and named in @requires. The router
// sends the external value in the representation, so the entity resolver
// reads it from there rather than from this subgraph's own data.
func TestRequiresRoundTripsThroughTheRepresentation(t *testing.T) {
	const sdl = `
type Product @key(fields: "sku") {
  sku: String!
  weight: Float! @external
  shippingCost: Float! @requires(fields: "weight")
}
type Query { hello: String! }
`
	type prod struct {
		SKU    string
		Weight float64
	}
	src, bindings, err := fed.Subgraph(sdl, fed.Resolver("Product",
		func(_ context.Context, r fed.Representation) (*prod, error) {
			sku, _ := r["sku"].(string)
			w, _ := r.Float("weight")
			return &prod{SKU: sku, Weight: w}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[prod]("Product",
			graphql.Field("sku", func(p *prod) string { return p.SKU }),
			graphql.Field("weight", func(p *prod) float64 { return p.Weight }),
			graphql.Field("shippingCost", func(p *prod) float64 { return p.Weight * 2 }),
		),
		graphql.Query(graphql.Resolve("hello", func(context.Context, graphql.Root) (string, error) {
			return "hi", nil
		})))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := entities(t, graphql.NewExecutor(s),
		`[{"__typename":"Product","sku":"abc","weight":3}]`,
		`... on Product { sku shippingCost }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"_entities":[{"sku":"abc","shippingCost":6}]}` {
		t.Fatalf("data = %s", got)
	}
}

// @key(resolvable: false) is how a subgraph refers to an entity another
// subgraph owns: it can name the key and cannot resolve it. Such a type was
// still required to have a resolver and still joined _Entity.
func TestAnUnresolvableKeyNeedsNoResolverAndIsNotAnEntity(t *testing.T) {
	const sdl = `
		type User @key(fields: "id") { id: ID! name: String! }
		type Product @key(fields: "sku", resolvable: false) { sku: String! }
		type Query { me: User }
	`
	src, bindings, err := fed.Subgraph(sdl, fed.Resolver("User", resolveUser))
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		graphql.Object[product]("Product", graphql.Field("sku", func(p *product) string { return p.SKU })),
		graphql.Query(graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) { return users["1"], nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	members := s.AST().Types["_Entity"].Types
	if len(members) != 1 || members[0] != "User" {
		t.Fatalf("_Entity = %v, want User alone", members)
	}

	_, _, err = fed.Subgraph(sdl, fed.Resolver("User", resolveUser), fed.Resolver("Product", resolveProduct))
	if err == nil || !strings.Contains(err.Error(), "Product") {
		t.Fatalf("err = %v, want a refusal of the resolver for Product, which this subgraph cannot resolve", err)
	}
}

// Resolver's type parameter is the Go type it returns, and nothing compared
// it with the Go type the entity is bound to. A resolver for User that returns
// *product built, and a User representation was answered as a Product: the
// _Entity union picks the object by Go type, so the wrong one was not even an
// error.
func TestAResolverMustReturnTheEntitysBoundGoType(t *testing.T) {
	wrong := func(context.Context, fed.Representation) (*product, error) { return &product{SKU: "not-a-user"}, nil }
	for name, entity := range map[string]fed.Entity{
		"Resolver": fed.Resolver("User", wrong),
		"BatchResolver": fed.BatchResolver("User", func(_ context.Context, reps []fed.Representation) ([]*product, error) {
			return make([]*product, len(reps)), nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			src, bindings, err := fed.Subgraph(subgraphSDL, entity, fed.Resolver("Product", resolveProduct))
			if err != nil {
				t.Fatalf("Subgraph: %v", err)
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
				graphql.Query(graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) { return users["1"], nil })),
			)
			if err == nil {
				t.Fatal("the schema built with a User resolver that returns a product")
			}
			for _, want := range []string{"User", "product", "user"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %s: %v", want, err)
				}
			}
		})
	}
}
