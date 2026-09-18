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
