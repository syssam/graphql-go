package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// An IDE, a client generator and a schema registry all send the same
// introspection query, and an API's depth, complexity and cost limits are
// set against its own queries. Introspection is not charged against them --
// it is bounded by the MaxIntrospectionDepth rule instead -- so a deployment with
// sane limits still answers its tools.
func TestIntrospectionIsNotChargedAgainstLimits(t *testing.T) {
	_, e := newFixtureExecutor(t,
		WithMaxDepth(3),
		WithMaxComplexity(5),
		WithQueryCost(QueryCost{Max: 10, DefaultListSize: 50, Report: true, Actual: true}),
	)
	resp := run(t, e, graphiqlIntrospection, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("the standard introspection query was refused: %s", errorsJSON(resp.Errors))
	}
	if !strings.Contains(string(resp.Data), `"queryType":{"name":"Query"}`) {
		t.Fatalf("no schema in the answer: %.200s", resp.Data)
	}
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if cost["requestedQueryCost"] != 0 || cost["actualQueryCost"] != 0 {
		t.Errorf("introspection was charged: %v", cost)
	}

	// Beside an ordinary field, only the ordinary field counts.
	resp = run(t, e, `{ me { id } __type(name: "User") { fields { name type { kind ofType { kind ofType { name } } } } } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("%s", errorsJSON(resp.Errors))
	}
	cost, _ = resp.Extensions["cost"].(map[string]any)
	if cost["requestedQueryCost"] != 2 || cost["actualQueryCost"] != 2 {
		t.Errorf("me { id } costs 2 and __type nothing: %v", cost)
	}

	// The limits still hold for everything else.
	for query, code := range map[string]string{
		`{ me { friends { bestFriend { id } } } }`:   CodeMaxDepth,
		`{ me { id name nick tags scores bigInt } }`: CodeTooComplex,
		`{ users { friends { id } } }`:               CodeTooComplex, // cost 1 + 50*(1+50)
	} {
		resp := run(t, e, query, "")
		if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != code {
			t.Errorf("%s: want %s, got %s", query, code, errorsJSON(resp.Errors))
		}
	}
}

// graphql-js's MaxIntrospectionDepthRule: a path below __schema or __type may
// pass through at most two of fields, interfaces, possibleTypes and
// inputFields, however it gets there.
func TestMaxIntrospectionDepth(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for name, tc := range map[string]struct {
		query   string
		refused bool
	}{
		"standard query":  {graphiqlIntrospection, false},
		"two list levels": {`{ __schema { types { fields { type { fields { name } } } } } }`, false},
		"three":           {`{ __schema { types { fields { type { fields { type { fields { name } } } } } } } }`, true},
		"from __type":     {`{ __type(name: "User") { fields { type { fields { type { fields { name } } } } } } }`, true},
		"mixed lists":     {`{ __schema { types { interfaces { possibleTypes { inputFields { name } } } } } }`, true},
		"through fragments": {`{ __schema { types { ...F } } }
			fragment F on __Type { fields { type { ...G } } }
			fragment G on __Type { fields { type { ...H } } }
			fragment H on __Type { fields { name } }`, true},
		"through inline fragments": {`{ __type(name: "User") { ... on __Type { fields { type { ... { inputFields { type { interfaces { name } } } } } } } } }`, true},
		// ofType is not a member list: a deep type reference is not a deep
		// introspection query.
		"deep ofType":                     {`{ __schema { types { fields { type { ofType { ofType { ofType { ofType { ofType { ofType { ofType { ofType { name } } } } } } } } } } } } }`, false},
		"an alias does not hide the name": {`{ __schema { types { a: fields { type { b: fields { type { c: fields { name } } } } } } } }`, true},
	} {
		t.Run(name, func(t *testing.T) {
			resp := run(t, e, tc.query, "")
			refused := len(resp.Errors) == 1 && strings.Contains(resp.Errors[0].Message, "Maximum introspection depth exceeded")
			if refused != tc.refused {
				t.Errorf("refused = %v, want %v: %s", refused, tc.refused, errorsJSON(resp.Errors))
			}
		})
	}
}

// Every fragment spreads the next twice: unmemoized, checking the document
// walks 2^40 paths.
func TestMaxIntrospectionDepthIsLinearInFragments(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const n = 40
	var b strings.Builder
	b.WriteString(`{ __schema { types { ...F0 } } }`)
	for i := range n {
		fmt.Fprintf(&b, "\nfragment F%d on __Type { name ...F%d ...F%d }", i, i+1, i+1)
	}
	fmt.Fprintf(&b, "\nfragment F%d on __Type { name }", n)
	done := make(chan *Response, 1)
	go func() { done <- run(t, e, b.String(), "") }()
	select {
	case resp := <-done:
		if len(resp.Errors) > 0 {
			t.Fatalf("%s", errorsJSON(resp.Errors))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("validating 41 fragments did not finish in 10s")
	}
}

// With introspection disabled the rule has nothing to bound, and the
// refusal is the one it always was.
func TestMaxIntrospectionDepthDefersToDisabled(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int }`), Query(Field("a", func(Root) int { return 1 })), DisableIntrospection())
	if err != nil {
		t.Fatal(err)
	}
	resp := NewExecutor(s).Execute(context.Background(), &Request{Query: `{ __schema { types { fields { type { fields { type { fields { name } } } } } } } }`})
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "introspection is not allowed") {
		t.Fatalf("%s", errorsJSON(resp.Errors))
	}
}
