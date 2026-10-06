package howto

import (
	"context"
	"strings"

	"github.com/syssam/graphql-go"
)

const billingSDL = `
directive @auth(requires: [String!]) on OBJECT | FIELD_DEFINITION
directive @authorizeObject on OBJECT

type Invoice @auth(requires: ["user"]) @authorizeObject {
  id: ID!
  total: Int!
  note: String @auth(requires: ["staff"])
}

type Query {
  invoices: [Invoice!]! @auth(requires: ["user"])
  invoice(id: ID!): Invoice @auth(requires: ["user"])
}
`

type Invoice struct {
	ID    string
	Owner string
	Total int
	Note  string
}

// Caller is who is asking. The zero Caller is anonymous: a nil map answers
// false for every scope, so a request nobody authenticated holds nothing.
type Caller struct {
	Name   string
	Scopes map[string]bool
}

type callerKey struct{}

// WithCaller is what a transport calls once, after it has verified a token.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// authorize runs once per operation, before any resolver. The shape lists
// every guarded position the query touches, and what is recorded for a
// position is what the client gets there.
func authorize(ctx context.Context, shape *graphql.AuthShape, d *graphql.Decision) error {
	held := callerFrom(ctx).Scopes
	for i, site := range shape.Sites() {
		// A row does not exist yet: AuthorizeObjects decides it, below.
		if site.Kind == graphql.SiteInstance || site.Requires.Satisfied(held) {
			continue
		}
		outcome := graphql.Deny(strings.Join(site.Requires.Scopes(), " and "), site.Coord)
		if site.Coord == "Invoice.note" {
			// Nullable, so it can be withheld without failing the invoice.
			outcome = graphql.Null()
		}
		if err := d.Set(i, outcome); err != nil {
			return err
		}
	}
	return nil
}

// invoiceRows decides which invoices a caller may see. It runs during
// execution, with the rows a resolver returned.
type invoiceRows struct{}

func (invoiceRows) AuthorizeObjects(ctx context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error) {
	caller := callerFrom(ctx)
	// One outcome per check, in the same order. The zero Outcome allows.
	outcomes := make([]graphql.Outcome, len(checks))
	for i, check := range checks {
		invoice, ok := check.Object.(*Invoice)
		switch {
		case ok && (caller.Scopes["staff"] || invoice.Owner == caller.Name):
			// Theirs to see.
		case check.Site.ListElement:
			outcomes[i] = graphql.Drop()
		case !check.Site.NonNull:
			outcomes[i] = graphql.Null()
		default:
			outcomes[i] = graphql.Deny("read", check.Type)
		}
	}
	return outcomes, nil
}

func NewBillingSchema(invoices []*Invoice) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(billingSDL),
		// Says which directive is a requirement and how its list reads:
		// AllOf means every scope named is needed.
		graphql.RequirementDirective("auth", "requires", graphql.ScopesAllOf),

		graphql.Object[Invoice]("Invoice",
			graphql.Field("id", func(i *Invoice) graphql.ID { return graphql.ID(i.ID) }),
			graphql.Field("total", func(i *Invoice) int { return i.Total }),
			graphql.Field("note", func(i *Invoice) *string { return &i.Note }),
		),
		graphql.Query(
			graphql.Resolve("invoices", func(context.Context, graphql.Root) ([]*Invoice, error) {
				return invoices, nil
			}),
			graphql.ResolveArgs("invoice", func(_ context.Context, _ graphql.Root, a invoiceArgs) (*Invoice, error) {
				for _, i := range invoices {
					if i.ID == string(a.ID) {
						return i, nil
					}
				}
				return nil, nil
			}),
		),
	)
}

type invoiceArgs struct{ ID graphql.ID }

// NewBillingExecutor attaches both halves. The directives only describe
// positions; these two options are what enforces them.
func NewBillingExecutor(schema *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(schema,
		graphql.WithAuthorizer(graphql.AuthorizerFunc(authorize)),
		graphql.WithObjectAuthorizer(invoiceRows{}),
	)
}
