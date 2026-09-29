package order

import (
	"context"
	"errors"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	vorder "github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/predicate"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/query"
	"github.com/syssam/velox/dialect/sql"
)

// errSignIn is what an anonymous caller gets for any order, read or written:
// UNAUTHENTICATED, which a client answers by signing in, where
// INTERNAL_SERVER_ERROR says the server is broken.
var errSignIn = apperr.New(apperr.Unauthenticated, "sign in to see or change orders")

// owned is the condition an order must meet for the viewer to touch it: none
// for staff, the viewer's own customer for a customer, and an error for
// nobody, failing closed rather than falling through to every order.
//
// It is a WHERE, not a check on each row after loading, so it is one indexed
// condition whatever the table's size -- and the same one for a read and for
// a write, which is the point: a filter on reads alone let a customer pay
// someone else's order and be told only that it was not found.
func owned(ctx context.Context) ([]predicate.Order, error) {
	id, all, err := ownerOf(ctx)
	if err != nil || all {
		return nil, err
	}
	return []predicate.Order{func(s *sql.Selector) {
		s.Where(sql.EQ(s.C(vorder.CustomerColumn), id))
	}}, nil
}

// ownerOf is whose orders the viewer may touch: every customer's for staff
// (all), their own for a customer, and an error for nobody.
func ownerOf(ctx context.Context) (customerID int, all bool, err error) {
	v := viewer.From(ctx)
	switch {
	case v.Staff:
		return 0, true, nil
	case v.Anonymous():
		return 0, false, errSignIn
	}
	return v.CustomerID, false, nil
}

// OwnOrders narrows every order read to the viewer's own: the list, a lookup
// by id, a customer's orders, and every eager load of an order edge all pass
// through it. Writes are narrowed where they are made (transition), since an
// interceptor sees only queries.
func OwnOrders(client *velox.Client) {
	client.Order.Intercept(velox.TraverseFunc(func(ctx context.Context, q velox.Query) error {
		ps, err := owned(ctx)
		if err != nil || len(ps) == 0 {
			return err
		}
		oq, ok := q.(*query.OrderQuery)
		if !ok {
			return errors.New("OwnOrders: unexpected query type")
		}
		oq.Where(ps...)
		return nil
	}))
}

// OwnOrderItems narrows every order-line read the same way: a line is the
// viewer's when its order is. Lines are reachable without an order -- the
// orderItems root, orderItem(id), Product.orderItems, an eager load -- so
// narrowing orders alone left every customer's purchases readable by anyone.
//
// The condition is written as the subquery it is, order IN (SELECT id FROM
// orders WHERE customer = ?), rather than through orderitem.HasOrderWith,
// which builds the same SQL through velox's generic graph steps: 21 fewer
// allocations and 1 KB less on a customer's fifty-order page, time not
// distinguishable (BenchmarkCustomerOrderHistory against HasOrderWith,
// interleaved n=12). The filter itself costs that page about 4% in
// allocations over no filter at all, which is the price of lines not being
// readable by everyone.
func OwnOrderItems(client *velox.Client) {
	client.OrderItem.Intercept(velox.TraverseFunc(func(ctx context.Context, q velox.Query) error {
		id, all, err := ownerOf(ctx)
		if err != nil || all {
			return err
		}
		iq, ok := q.(*query.OrderItemQuery)
		if !ok {
			return errors.New("OwnOrderItems: unexpected query type")
		}
		iq.Where(func(s *sql.Selector) {
			orders := sql.Table(vorder.Table)
			s.Where(sql.In(s.C(orderitem.OrderColumn),
				sql.Select(orders.C(vorder.FieldID)).From(orders).Where(sql.EQ(orders.C(vorder.CustomerColumn), id))))
		})
		return nil
	}))
}
