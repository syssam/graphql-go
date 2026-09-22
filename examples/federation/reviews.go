package federation

import (
	"context"
	"errors"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

// ReviewsSDL is the reviews subgraph. It owns Review and contributes two
// fields to a Product it does not own.
//
// The half worth reading is Product. This service declares the same @key and
// the same sku field as the products subgraph, and nothing else of it: sku is
// here because the key has to exist on the type, not because this service has
// anything to say about it. Everything the router asks of Product here
// arrives through _entities as a representation carrying that key.
const ReviewsSDL = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", import: ["@key", "@external"])

type Product @key(fields: "sku") {
  sku: ID! @external
  reviews: [Review!]!
  averageRating: Float
}

type Review @key(fields: "id") {
  id: ID!
  body: String!
  rating: Int!
}

type Query {
  latestReviews(first: Int! = 3): [Review!]!
}
`

// Review is the entity this subgraph owns.
type Review struct {
	ID     string
	SKU    string
	Body   string
	Rating int
}

// ProductRef is this subgraph's view of a Product: the key, and nothing else.
// A representation the router sends carries only the key fields, so this is
// the whole of what a resolver here has to work with -- which is the point of
// the type being this small rather than an oversight.
type ProductRef struct{ SKU string }

var reviewRows = []*Review{
	{ID: "r1", SKU: "kb-01", Body: "Clicky.", Rating: 5},
	{ID: "r2", SKU: "kb-01", Body: "Loud.", Rating: 3},
	{ID: "r3", SKU: "mn-27", Body: "Sharp.", Rating: 4},
}

type latestReviewsArgs struct{ First int }

// NewReviews builds the reviews subgraph.
func NewReviews() (*graphql.Executor, error) {
	src, bindings, err := fed.Subgraph(ReviewsSDL,
		// Product is resolved from the key alone. There is no fetch here and
		// there should not be: this service knows nothing about a product
		// except its sku, and inventing a lookup would be inventing a
		// dependency on the service that owns it -- which is the coupling
		// federation exists to remove.
		fed.Resolver("Product", func(_ context.Context, r fed.Representation) (*ProductRef, error) {
			sku, ok := r.ID("sku")
			if !ok {
				return nil, errors.New("Product representation has no sku")
			}
			return &ProductRef{SKU: string(sku)}, nil
		}),
		fed.Resolver("Review", func(_ context.Context, r fed.Representation) (*Review, error) {
			id, ok := r.ID("id")
			if !ok {
				return nil, errors.New("Review representation has no id")
			}
			return findReview(string(id)), nil
		}),
	)
	if err != nil {
		return nil, err
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[ProductRef]("Product",
			graphql.Field("sku", func(p *ProductRef) graphql.ID { return graphql.ID(p.SKU) }),
			graphql.Field("reviews", func(p *ProductRef) []*Review { return reviewsOf(p.SKU) }),
			graphql.Field("averageRating", func(p *ProductRef) *float64 {
				rs := reviewsOf(p.SKU)
				if len(rs) == 0 {
					// Nullable on purpose: no reviews is not a rating of
					// zero, and a zero here would be a wrong number rather
					// than a missing one.
					return nil
				}
				var sum int
				for _, r := range rs {
					sum += r.Rating
				}
				avg := float64(sum) / float64(len(rs))
				return &avg
			}),
		),
		graphql.Object[Review]("Review",
			graphql.Field("id", func(r *Review) graphql.ID { return graphql.ID(r.ID) }),
			graphql.Field("body", func(r *Review) string { return r.Body }),
			graphql.Field("rating", func(r *Review) int { return r.Rating }),
		),
		graphql.Args[latestReviewsArgs](
			graphql.InputField("first", func(a *latestReviewsArgs, v int) { a.First = v }),
		),
		graphql.Query(
			graphql.ResolveArgs("latestReviews", func(_ context.Context, _ graphql.Root, a latestReviewsArgs) ([]*Review, error) {
				n := min(a.First, len(reviewRows))
				out := make([]*Review, 0, n)
				out = append(out, reviewRows[:n]...)
				return out, nil
			}),
		),
	)
	if err != nil {
		return nil, err
	}
	return graphql.NewExecutor(s), nil
}

func reviewsOf(sku string) []*Review {
	out := make([]*Review, 0, 2)
	for _, r := range reviewRows {
		if r.SKU == sku {
			out = append(out, r)
		}
	}
	return out
}

func findReview(id string) *Review {
	for _, r := range reviewRows {
		if r.ID == id {
			return r
		}
	}
	return nil
}
