// Package viewer is who is asking: a staff member, a signed-in customer, or
// nobody. It is set on the request context once, by Middleware, and read by
// the authorization policy and by the database's read filter.
package viewer

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// Scope names. The SDL spells them in @requiresScopes and the policy reads
// them back; no role name appears in the schema.
const (
	// ScopePII guards customer personal data. Without it an email is
	// masked, which still tells two customers apart.
	ScopePII = "customer:pii"
	// ScopeInventory guards stock levels. Without it a product's stocks are
	// an empty list, and velox does not query them.
	ScopeInventory = "inventory:read"
)

// Viewer is the caller. CustomerID is set for a signed-in customer, who sees
// only their own orders; it is zero for staff and for an anonymous caller.
type Viewer struct {
	Staff      bool
	CustomerID int
}

// Anonymous reports whether nobody signed in.
func (v Viewer) Anonymous() bool { return !v.Staff && v.CustomerID == 0 }

// Scopes returns what the viewer holds. Staff hold every scope; a customer
// and an anonymous caller hold none.
func (v Viewer) Scopes() map[string]bool {
	if !v.Staff {
		return nil
	}
	return map[string]bool{ScopePII: true, ScopeInventory: true}
}

type ctxKey struct{}

// With returns ctx carrying v.
func With(ctx context.Context, v Viewer) context.Context {
	return context.WithValue(ctx, ctxKey{}, v)
}

// From returns the viewer in ctx; an anonymous one when there is none.
func From(ctx context.Context) Viewer {
	v, _ := ctx.Value(ctxKey{}).(Viewer)
	return v
}

// Header names the caller. It stands in for authentication: "staff", or
// "customer:<id>". A real deployment verifies a token here and changes
// nothing downstream, which reads only the Viewer.
const Header = "X-Viewer"

// Middleware puts the request's viewer on its context. A malformed header is
// refused rather than read as anonymous, so a typo fails loudly.
func Middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		v, ok := Parse(c.Request().Header.Get(Header))
		if !ok {
			return echo.NewHTTPError(http.StatusUnauthorized, "malformed "+Header)
		}
		c.SetRequest(c.Request().WithContext(With(c.Request().Context(), v)))
		return next(c)
	}
}

// Parse reads a Header value; "" is anonymous.
func Parse(h string) (Viewer, bool) {
	switch {
	case h == "":
		return Viewer{}, true
	case h == "staff":
		return Viewer{Staff: true}, true
	case strings.HasPrefix(h, "customer:"):
		id, err := strconv.Atoi(strings.TrimPrefix(h, "customer:"))
		if err != nil || id <= 0 {
			return Viewer{}, false
		}
		return Viewer{CustomerID: id}, true
	}
	return Viewer{}, false
}
