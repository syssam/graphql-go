package graphql

import (
	"strings"
	"testing"
)

// A weight key that names nothing used to price nothing, silently: the field fell
// back to weight 1, so a typo or a renamed field left an expensive field cheap with
// no sign anywhere. Each shape of that mistake is its own message.
func TestQueryCostValidateNamesEveryKeyThatPricesNothing(t *testing.T) {
	t.Parallel()
	s, err := NewSchema(SDL(fixtureSDL), newFixture().options()...)
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]int{"Query.users": 5, "Mutation.inc": 2, "User.friends": 10, "Post.author": 3}
	if err := (QueryCost{FieldWeight: valid}).Validate(s); err != nil {
		t.Fatalf("keys naming object fields priced nothing wrongly: %v", err)
	}
	if err := (QueryCost{}).Validate(s); err != nil {
		t.Fatalf("no weights is not a mistake: %v", err)
	}
	if err := (QueryCost{FieldWeight: map[string]int{"Nope.x": 1}}).Validate(nil); err != nil {
		t.Fatalf("a nil schema has nothing to check against: %v", err)
	}

	for key, want := range map[string]string{
		"Query.usres":        "names no field of Query",
		"Qeury.users":        "names no type",
		"Node.id":            "names an interface",
		"SearchResult.title": "names a union",
		"Role.ADMIN":         "names an enum",
		"__Schema.types":     "introspection",
		"users":              `"Type.field"`,
		".users":             `"Type.field"`,
		"Query.":             `"Type.field"`,
	} {
		err := (QueryCost{FieldWeight: map[string]int{key: 9}}).Validate(s)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `"`+key+`"`) {
			t.Errorf("key %q: want an error naming it and saying %q, got %v", key, want, err)
		}
	}

	// Every bad key is reported, so one run fixes them all, and a good key beside
	// them is not.
	err = (QueryCost{FieldWeight: map[string]int{"Query.usres": 1, "Query.users": 1, "Mutation.nc": 1}}).Validate(s)
	if err == nil {
		t.Fatal("two bad keys went unreported")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Query.usres") || !strings.Contains(msg, "Mutation.nc") || strings.Contains(msg, `"Query.users"`) {
		t.Errorf("want both bad keys and not the good one:\n%s", msg)
	}
	if strings.Index(msg, "Mutation.nc") > strings.Index(msg, "Query.usres") {
		t.Errorf("keys should be reported in sorted order:\n%s", msg)
	}
}

// The mistake is also said at start-up, once per process like the unbounded
// warning, for the service that never calls Validate.
func TestNewExecutorSaysWhenWeightKeysPriceNothing(t *testing.T) {
	s, err := NewSchema(SDL(fixtureSDL), newFixture().options()...)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		weights map[string]int
		want    int
	}{
		"a typo":        {map[string]int{"Query.usres": 5}, 1},
		"a valid key":   {map[string]int{"Query.users": 5}, 0},
		"no weights":    {nil, 0},
		"several typos": {map[string]int{"Query.a": 1, "Query.b": 1}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			costKeysSaid.Store(false)
			logged := captureLog(t)
			NewExecutor(s, WithQueryCost(QueryCost{FieldWeight: tc.weights}))
			NewExecutor(s, WithQueryCost(QueryCost{FieldWeight: tc.weights}))
			if got := strings.Count(logged.String(), "price no field"); got != tc.want {
				t.Errorf("said %d time(s) for two executors, want %d:\n%s", got, tc.want, logged)
			}
		})
	}
}
