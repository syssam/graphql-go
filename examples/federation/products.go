// Package federation is two Apollo Federation subgraphs and the fetch a
// router performs across them.
//
// The fed package's own tests prove one subgraph answers `_service` and
// `_entities` correctly. What they cannot show is the thing federation
// actually is: an entity owned by one service and extended by another, joined
// at request time by a router that neither service knows about. That join is
// where a subgraph implementation is usually wrong -- the `__typename` it
// forgets to select, the key it returns in the wrong scalar type, the
// representation it cannot read back -- and it needs two subgraphs to show at
// all.
//
// `federation_test.go` plays the router: it reads both subgraphs' SDL, runs
// the two-phase fetch by hand and merges the results. Building a real router
// is out of scope and a much larger problem than serving a subgraph; playing
// one for a known query is not, and it is enough to prove the contract holds
// from both sides.
package federation

import (
	"context"
	"errors"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

// ProductsSDL is the products subgraph: it owns Product.
//
// The author's text is the contract. fed.Subgraph adds the protocol
// scaffolding no author writes in any implementation -- the directive
// declarations, _Any, _Service, the _Entity union and the two root fields --
// and returns this text verbatim from _service, because this is what the
// router composes from.
const ProductsSDL = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", import: ["@key"])

type Product @key(fields: "sku") {
  sku: ID!
  name: String!
  price: Int!
}

type Query {
  topProducts(first: Int! = 3): [Product!]!
  product(sku: ID!): Product
}
`

// Product is the entity this subgraph owns.
type Product struct {
	SKU   string
	Name  string
	Price int
}

var productRows = []*Product{
	{SKU: "kb-01", Name: "Keyboard", Price: 1299},
	{SKU: "mn-27", Name: "Monitor", Price: 24900},
	{SKU: "cb-02", Name: "Cable", Price: 1775},
}

type topProductsArgs struct{ First int }

type productArgs struct{ SKU graphql.ID }

// NewProducts builds the products subgraph.
func NewProducts() (*graphql.Executor, error) {
	src, bindings, err := fed.Subgraph(ProductsSDL,
		// The router calls this with one representation per product it needs
		// back. A key it cannot resolve is a null in place, not an error:
		// the router asked about something this service does not have.
		fed.Resolver("Product", func(_ context.Context, r fed.Representation) (*Product, error) {
			sku, ok := r.ID("sku")
			if !ok {
				return nil, errors.New("Product representation has no sku")
			}
			return findProduct(string(sku)), nil
		}),
	)
	if err != nil {
		return nil, err
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[Product]("Product",
			graphql.Field("sku", func(p *Product) graphql.ID { return graphql.ID(p.SKU) }),
			graphql.Field("name", func(p *Product) string { return p.Name }),
			graphql.Field("price", func(p *Product) int { return p.Price }),
		),
		graphql.Args[topProductsArgs](
			graphql.InputField("first", func(a *topProductsArgs, v int) { a.First = v }),
		),
		graphql.Args[productArgs](
			graphql.InputField("sku", func(a *productArgs, v graphql.ID) { a.SKU = v }),
		),
		graphql.Query(
			graphql.ResolveArgs("topProducts", func(_ context.Context, _ graphql.Root, a topProductsArgs) ([]*Product, error) {
				return productRows[:min(a.First, len(productRows))], nil
			}),
			graphql.ResolveArgs("product", func(_ context.Context, _ graphql.Root, a productArgs) (*Product, error) {
				return findProduct(string(a.SKU)), nil
			}),
		),
	)
	if err != nil {
		return nil, err
	}
	return graphql.NewExecutor(s), nil
}

func findProduct(sku string) *Product {
	for _, p := range productRows {
		if p.SKU == sku {
			return p
		}
	}
	return nil
}
