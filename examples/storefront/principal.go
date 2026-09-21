package storefront

import "context"

// Scope names. They are the vocabulary the SDL spells out at each position
// and the Authorizer reads back; nothing in the schema knows about roles.
const (
	// ScopeAuthenticated is what the @authenticated marker directive
	// requires. Every signed-in principal holds it and an anonymous one
	// does not, which is the whole of what the marker means here.
	ScopeAuthenticated = "authenticated"
	// ScopeCustomerPII guards personal data.
	ScopeCustomerPII = "customer:pii"
	// ScopeFinanceRead guards margin, both as a field and as something to
	// filter by.
	ScopeFinanceRead = "finance:read"
	// ScopeOrderWrite guards the refund mutation.
	ScopeOrderWrite = "order:write"
	// ScopeOrderReadAny lifts the per-row ownership check. A principal
	// without it sees only its own orders, however it asks.
	ScopeOrderReadAny = "order:read:any"
)

// Principal is who is asking. CustomerID is set for a customer signing in to
// their own account and empty for internal staff; scopes are what an
// authenticated session was granted.
//
// It is a value, not an interface, because this example authenticates from a
// header. A real deployment would put the claims of a verified token here and
// change nothing below.
type Principal struct {
	Subject    string
	CustomerID string
	scopes     map[string]bool
}

// NewPrincipal returns a principal holding the named scopes plus
// ScopeAuthenticated, which signing in is what confers.
func NewPrincipal(subject, customerID string, scopes ...string) Principal {
	held := make(map[string]bool, len(scopes)+1)
	held[ScopeAuthenticated] = true
	for _, s := range scopes {
		held[s] = true
	}
	return Principal{Subject: subject, CustomerID: customerID, scopes: held}
}

// Staff is an internal user who may read every order, its margin and
// customer personal data, and may refund.
func Staff(subject string) Principal {
	return NewPrincipal(subject, "", ScopeOrderReadAny, ScopeCustomerPII, ScopeFinanceRead, ScopeOrderWrite)
}

// Support is an agent who may read every order but neither personal data nor
// margin. It is the principal worth reading the tests for: everything it
// cannot see, it cannot see by a different mechanism.
func Support(subject string) Principal {
	return NewPrincipal(subject, "", ScopeOrderReadAny)
}

// CustomerPrincipal is a customer signed in to their own account. It holds no
// scope beyond authentication, so what it may see is decided per row.
func CustomerPrincipal(customerID string) Principal {
	return NewPrincipal("customer:"+customerID, customerID)
}

// Anonymous is an unauthenticated caller. It holds nothing, so every position
// but Query.health refuses it.
func Anonymous() Principal { return Principal{scopes: map[string]bool{}} }

// Holds reports whether the principal was granted a scope.
func (p Principal) Holds(scope string) bool { return p.scopes[scope] }

// Scopes returns what the principal holds, in the form Requirement.Satisfied
// takes. The map is the principal's own and must not be written to.
func (p Principal) Scopes() map[string]bool { return p.scopes }

type principalKey struct{}

// WithPrincipal returns ctx carrying p. A transport calls this once, after
// authenticating; nothing below reads a header again.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal on ctx, or Anonymous. Defaulting to
// Anonymous rather than to a zero Principal is deliberate: a request that
// never went through WithPrincipal must be refused everywhere, not admitted
// by a nil map that happens to answer false to every question -- which is the
// same answer, but by accident rather than by decision.
func PrincipalFrom(ctx context.Context) Principal {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return Anonymous()
	}
	return p
}
