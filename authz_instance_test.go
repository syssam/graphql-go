package graphql

import (
	"strings"
	"testing"
)

func TestObjectAuthBatchDefaultsAndOverrides(t *testing.T) {
	_, e := newFixtureExecutor(t)
	if e.objectAuthBatch != 50 {
		t.Fatalf("default batch = %d, want 50", e.objectAuthBatch)
	}
	_, e = newFixtureExecutor(t, WithObjectAuthBatch(7))
	if e.objectAuthBatch != 7 {
		t.Fatalf("batch = %d, want 7", e.objectAuthBatch)
	}
	// A non-positive size is the caller asking for no splitting at all, which
	// would defeat the bound: keep the default rather than silently accepting.
	_, e = newFixtureExecutor(t, WithObjectAuthBatch(0))
	if e.objectAuthBatch != 50 {
		t.Fatalf("batch = %d, want 50 for a non-positive size", e.objectAuthBatch)
	}
}

const authzObjectSDL = `
directive @authorizeObject on OBJECT
type Customer @authorizeObject { id: ID! name: String! }
type Query { customers: [Customer!]! }
`

// The valid cases below declare @authorizeObject with a wider location list
// than D1's `on OBJECT` alone: with only OBJECT declared, gqlparser's own
// location check rejects the bad placements before validateObjectDirectives
// ever runs, and the test would prove nothing about this validator.
func TestAuthorizeObjectPlacementAtBuild(t *testing.T) {
	cases := []struct {
		name    string
		sdl     string
		wantErr string
	}{
		{
			name: "on an object type",
			sdl:  authzObjectSDL,
		},
		{
			name:    "on an interface",
			sdl:     "directive @authorizeObject on OBJECT | INTERFACE\ninterface Node @authorizeObject { id: ID! }\ntype Customer implements Node { id: ID! }\ntype Query { c: Customer! }",
			wantErr: "Node: @authorizeObject is valid only on an object type, not an interface",
		},
		{
			name:    "on a field",
			sdl:     "directive @authorizeObject on OBJECT | FIELD_DEFINITION\ntype Query { c: String @authorizeObject }",
			wantErr: "Query.c: @authorizeObject is valid only on an object type, not a field",
		},
		{
			name:    "on an input object",
			sdl:     "directive @authorizeObject on OBJECT | INPUT_OBJECT\ninput Where @authorizeObject { name: String }\ntype Query { c(w: Where): String }",
			wantErr: "Where: @authorizeObject is valid only on an object type, not an input object",
		},
		{
			name:    "on a union",
			sdl:     "directive @authorizeObject on OBJECT | UNION\ntype A { id: ID! }\ntype B { id: ID! }\nunion AB @authorizeObject = A | B\ntype Query { c: AB }",
			wantErr: "AB: @authorizeObject is valid only on an object type, not a union",
		},
		{
			name:    "more than once on one type",
			sdl:     "directive @authorizeObject repeatable on OBJECT\ntype Customer @authorizeObject @authorizeObject { id: ID! }\ntype Query { c: Customer }",
			wantErr: "Customer: @authorizeObject must not occur more than once",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(c.sdl))
			if c.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "authorizeObject") {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

type authzInstanceCustomer struct {
	ID   string
	Name string
}

func TestAuthorizeObjectMarksTheType(t *testing.T) {
	s, err := NewSchema(SDL(authzObjectSDL),
		Object[authzInstanceCustomer]("Customer",
			Field("id", func(*authzInstanceCustomer) ID { return "" }),
			Field("name", func(*authzInstanceCustomer) string { return "" }),
		),
		Query(Field("customers", func(Root) []authzInstanceCustomer { return nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	if !s.objects["Customer"].instanceGuarded {
		t.Fatal("Customer is not marked instance-guarded")
	}
	if s.objects["Query"].instanceGuarded {
		t.Fatal("Query is marked instance-guarded")
	}
}

func TestInstanceSiteAdmitsOnlyAllowNullDenyDrop(t *testing.T) {
	site := AuthSite{Coord: "Customer", Kind: SiteInstance}
	for _, o := range []Outcome{Allow(), Null(), Deny("read", "Customer"), Drop()} {
		if err := o.validFor(site); err != nil {
			t.Fatalf("outcome rejected for an instance site: %v", err)
		}
	}
	for name, o := range map[string]Outcome{
		"Zero":   Zero(),
		"Redact": Redact(func(v any) any { return v }),
	} {
		err := o.validFor(site)
		if err == nil {
			t.Fatalf("%s accepted for an instance site", name)
		}
		if !strings.Contains(err.Error(), "Customer") {
			t.Fatalf("%s error does not name the coordinate: %v", name, err)
		}
		// The pre-existing Zero/Redact cases in validFor's switch also reject
		// a nil Field, which every instance site has, so a substring check on
		// the coordinate alone would pass even without the SiteInstance rule.
		// Requiring "instance site" in the message pins the rejection to that
		// rule rather than to the coincidence.
		if !strings.Contains(err.Error(), "instance site") {
			t.Fatalf("%s error does not identify the site as an instance site: %v", name, err)
		}
	}
}
