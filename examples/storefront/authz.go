package storefront

import (
	"context"
	"slices"
	"strings"

	graphql "github.com/syssam/graphql-go"
)

// Policy answers both halves of the authorization contract.
//
// Authorize runs once per operation, before anything resolves, and decides
// the positions a query asks for -- a scope on a field, and the contents of
// a filter argument. AuthorizeObjects runs during execution and decides
// individual rows, because which orders a customer may see is a property of
// the rows and not of the query.
//
// Both are deliberately pure functions of the principal and the input. A real
// deployment would call OPA, Cedar, OpenFGA or a database here; the shape of
// the two methods is what such a backend has to be adapted to, and the point
// of AuthorizeObjects taking a slice is that the adapter gets one round trip
// per list rather than one per row.
type Policy struct{}

// Authorize implements graphql.Authorizer.
func (Policy) Authorize(ctx context.Context, shape *graphql.AuthShape, d *graphql.Decision) error {
	p := PrincipalFrom(ctx)
	held := p.Scopes()

	for i, site := range shape.Sites() {
		switch site.Kind {
		case graphql.SiteInputWrite:
			// Ordering on someone else's behalf is a staff action. The rule
			// reads a key path, not a value, because that is all
			// Decision.Input reports -- so the schema makes the key itself
			// the signal and lets an omitted customerId mean "for myself".
			if held[ScopeOrderWrite] || !mentions(d.Input(i), "customerId") {
				continue
			}
			if err := d.Set(i, graphql.Deny(ScopeOrderWrite, site.Coord)); err != nil {
				return err
			}

		case graphql.SiteFilterArg:
			// A filter is not covered by the requirement on the field it
			// filters: Order.margin needs finance:read to select, and
			// OrderWhere.minMargin needs it to compare, or a caller who
			// cannot read the number can still binary-search it.
			if held[ScopeFinanceRead] {
				continue
			}
			if !mentions(d.Input(i), "minMargin") {
				continue
			}
			if err := d.Set(i, graphql.Deny(ScopeFinanceRead, site.Coord)); err != nil {
				return err
			}

		case graphql.SiteInstance:
			// Decided by AuthorizeObjects during execution. Decision.Set
			// rejects an instance site outright, so this is not merely
			// pointless but an error.
			continue

		default:
			if site.Requires.Satisfied(held) {
				continue
			}
			if err := d.Set(i, withholding(site)); err != nil {
				return err
			}
		}
	}
	return nil
}

// withholding picks how a position the principal cannot have is withheld.
// The choice is per position and is a product decision, not a mechanical one:
// masking says "there is an address here and it is not yours to read", which
// is what a support agent needs to tell two customers apart, while refusing
// says nothing at all, which is what a number nobody may approximate needs.
func withholding(site graphql.AuthSite) graphql.Outcome {
	if site.Coord == "Customer.email" {
		return graphql.Redact(maskEmail)
	}
	return graphql.Deny(strings.Join(site.Requires.Scopes(), " and "), site.Coord)
}

// maskEmail keeps the shape of an address and none of its content. The
// argument is the resolved value, so the field still ran: Redact rewrites a
// result rather than preventing one, which is why it is wrong for anything
// whose computation is itself the secret.
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

// mentions reports whether the client supplied the named input field
// anywhere in an argument. Decision.Input reports key paths, enum values and
// explicit nulls -- never scalar values -- so the policy can see that
// minMargin was used without the engine handing it the amount.
func mentions(keys []graphql.InputKey, field string) bool {
	for _, k := range keys {
		if slices.Contains(k.Path, field) {
			return true
		}
	}
	return false
}

// AuthorizeObjects implements graphql.ObjectAuthorizer: row-level
// authorization for the @authorizeObject types.
//
// The whole list arrives in one call, which is the point -- a policy backend
// sees one request per list rather than one per row. The outcomes are
// positional over checks and must be the same length; a short slice would
// silently allow the rows it does not cover, so the engine treats it as an
// error.
func (Policy) AuthorizeObjects(ctx context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error) {
	p := PrincipalFrom(ctx)
	out := make([]graphql.Outcome, len(checks))

	for i, c := range checks {
		out[i] = graphql.Allow()
		if p.Holds(ScopeOrderReadAny) {
			continue
		}
		owner, ok := ownerOf(c.Object)
		if !ok {
			// A guarded type this policy does not recognise. Failing closed
			// is the only safe answer: a type added to the schema with
			// @authorizeObject and forgotten here must stop being visible,
			// not start being public.
			out[i] = graphql.Deny("order:read", c.Type)
			continue
		}
		if owner != "" && owner == p.CustomerID {
			continue
		}
		// Drop in a list, Deny anywhere else. Dropping is what makes a
		// listing show only your own rows without telling you how many
		// others exist; it is valid only at a list element position, and a
		// single non-null position has nothing to drop into.
		if c.Site.ListElement {
			out[i] = graphql.Drop()
			continue
		}
		out[i] = graphql.Deny("order:read", c.Type)
	}
	return out, nil
}

// ownerOf returns the customer a guarded value belongs to. The second result
// distinguishes "belongs to nobody" from "this policy does not know what this
// is", which decide differently.
func ownerOf(v any) (string, bool) {
	switch o := v.(type) {
	case *Order:
		return o.CustomerID, true
	case *Customer:
		return o.ID, true
	default:
		return "", false
	}
}
