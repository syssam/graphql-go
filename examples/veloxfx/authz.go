package veloxfx

import (
	"context"
	"errors"
	"strings"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/query"
	"github.com/syssam/velox/dialect/sql"
)

// Two layers, each doing what only it can:
//
//   - Positions (a field anyone may select, but only some may read) are the
//     engine's: schema/*.go puts @requiresScopes on them, and policy decides
//     once per operation, before anything resolves. That decision reaches
//     velox too -- a withheld position is neither selected nor loaded.
//   - Rows (which orders are yours) are the database's: ownOrders narrows
//     every order read in SQL, so a customer's page costs the same query
//     whether the table holds their ten orders or a million of others'.

// policy withholds each position the viewer lacks the scope for, the way
// that position needs: an email is masked -- except a customer's own, which
// is a question about the row the decision is made before, so RedactRow
// answers it per row -- and a masked address still tells two customers
// apart; stock levels become an empty list, which is not a lie a shopper can
// act on and costs no query; anything else is refused.
var policy = graphql.AuthorizerFunc(func(ctx context.Context, shape *graphql.AuthShape, d *graphql.Decision) error {
	held := viewer.From(ctx).Scopes()
	for i, site := range shape.Sites() {
		if site.Kind == graphql.SiteInstance || site.Requires.Satisfied(held) {
			continue
		}
		var o graphql.Outcome
		switch site.Coord {
		case "Customer.email":
			o = graphql.RedactRow(maskUnlessOwn)
		case "Product.stocks":
			o = graphql.Zero()
		default:
			o = graphql.Deny(strings.Join(site.Requires.Scopes(), " and "), site.Coord)
		}
		if err := d.Set(i, o); err != nil {
			return err
		}
	}
	return nil
})

// maskUnlessOwn shows a signed-in customer their own address and masks
// every other. A row it cannot judge is masked: it fails closed.
func maskUnlessOwn(ctx context.Context, parent, v any) any {
	if c, ok := parent.(*entity.Customer); ok && c.ID != 0 && c.ID == viewer.From(ctx).CustomerID {
		return v
	}
	return maskEmail(v)
}

// maskEmail keeps an address's shape and none of its content.
func maskEmail(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	local, domain, found := strings.Cut(s, "@")
	if !found || local == "" {
		return "***"
	}
	return local[:1] + strings.Repeat("*", len(local)-1) + "@" + domain
}

// errSignIn is what an anonymous order read gets: UNAUTHENTICATED, which a
// client answers by signing in, where INTERNAL_SERVER_ERROR says the server
// is broken.
var errSignIn = (&graphql.Error{Message: "sign in to read orders"}).WithExtension("code", "UNAUTHENTICATED")

// ownOrders narrows every order read to the viewer's own: the list, a
// lookup by id, a customer's orders, and every eager load of an order edge
// all pass through it. Staff read every order; an anonymous caller reads
// none, failing closed rather than falling through to all of them.
//
// It is a filter on the query, not a check on each row after loading, so it
// is one indexed WHERE whatever the table's size.
func ownOrders(client *velox.Client) {
	client.Order.Intercept(velox.TraverseFunc(func(ctx context.Context, q velox.Query) error {
		v := viewer.From(ctx)
		switch {
		case v.Staff:
			return nil
		case v.Anonymous():
			return errSignIn
		}
		oq, ok := q.(*query.OrderQuery)
		if !ok {
			return errors.New("ownOrders: unexpected query type")
		}
		oq.Where(func(s *sql.Selector) {
			s.Where(sql.EQ(s.C(order.CustomerColumn), v.CustomerID))
		})
		return nil
	}))
}
