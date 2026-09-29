package veloxfx

import (
	"context"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Three layers, each doing what only it can:
//
//   - Positions (a field anyone may select, but only some may read) are the
//     engine's: schema/*.go puts @requiresScopes on them, and policy decides
//     once per operation, before anything resolves. That decision reaches
//     velox too -- a withheld position is neither selected nor loaded.
//   - Rows (which orders and lines are yours, which stock you may count) are
//     the database's: order.OwnOrders, order.OwnOrderItems and
//     stock.HideLevels narrow every read in SQL -- roots, lookups, edges and
//     eager loads alike -- and each order write carries the same condition,
//     so a customer's page costs the same query whether the table holds
//     their ten orders or a million of others'.
//   - Operations (which mutations you may run) are mutationGate's, decided
//     from the document before anything runs.

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

// customerMutations are the mutations a signed-in customer may run, and the
// order service checks that the order each one touches is theirs. Every
// other mutation is staff's: one added to the SDL is refused to customers
// until it is listed here, where the opposite default leaves it open until
// someone remembers to guard it.
var customerMutations = map[string]bool{
	"placeOrder":  true,
	"payOrder":    true,
	"cancelOrder": true,
}

// mutationGate refuses a mutation the viewer may not run before any of it
// runs. It is not @requiresScopes because velox generates half the mutations
// and has no way to put a directive on them; an interceptor sees them all.
var mutationGate = graphql.OperationInterceptorFunc(func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
	v := viewer.From(ctx)
	if oc.Operation.Operation != ast.Mutation || v.Staff {
		return next(ctx, oc)
	}
	for _, name := range operationRootFields(oc.Doc, oc.Operation) {
		switch {
		case name == "__typename":
		case v.Anonymous():
			return refuse(apperr.New(apperr.Unauthenticated, "sign in to run %s", name))
		case !customerMutations[name]:
			return refuse(apperr.New(apperr.Forbidden, "%s is for staff", name))
		}
	}
	return next(ctx, oc)
})

// operationRootFields names every field op selects at its root.
func operationRootFields(doc *ast.QueryDocument, op *ast.OperationDefinition) []string {
	return rootFields(doc, op.SelectionSet, map[string]bool{})
}

// rootFields names every field a selection set selects, through
// fragments. @skip and @include are not evaluated: a field that might run is
// judged as if it will, so a variable cannot slip one past the gate. seen is
// shared by the whole walk, so each named fragment is expanded once however
// many times, and through however many inline fragments, it is spread.
func rootFields(doc *ast.QueryDocument, set ast.SelectionSet, seen map[string]bool) []string {
	var names []string
	for _, sel := range set {
		switch sel := sel.(type) {
		case *ast.Field:
			names = append(names, sel.Name)
		case *ast.InlineFragment:
			names = append(names, rootFields(doc, sel.SelectionSet, seen)...)
		case *ast.FragmentSpread:
			if seen[sel.Name] {
				continue
			}
			seen[sel.Name] = true
			if f := doc.Fragments.ForName(sel.Name); f != nil {
				names = append(names, rootFields(doc, f.SelectionSet, seen)...)
			}
		}
	}
	return names
}

func refuse(err *graphql.Error) *graphql.Response {
	return &graphql.Response{Errors: []*graphql.Error{err}}
}
