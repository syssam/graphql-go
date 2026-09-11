package graphql

import (
	"context"
	"encoding/json"
	"testing"
)

// FuzzExecute feeds arbitrary documents and variables to the executor. The
// invariant is that execution never panics and always produces a valid JSON
// envelope; both accepted and rejected inputs are fine.
func FuzzExecute(f *testing.F) {
	fx := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), fx.options()...)
	if err != nil {
		f.Fatal(err)
	}
	e := NewExecutor(s, WithMaxConcurrency(4))

	seeds := []struct{ query, vars string }{
		{`{ me { id name friends { id } } }`, ``},
		{`query($id: ID!) { user(id: $id) { id pet { name ... on Dog { barks } } } }`, `{"id":"1"}`},
		{`query($f: Filter) { users(filter: $f) { id } }`, `{"f":{"limit":1,"name":null}}`},
		{`query($a: Boolean!) { me { id @include(if: $a) nick @skip(if: $a) } }`, `{"a":true}`},
		{`{ echo(v: 1, s: "x", f: 1.5, b: true, list: [1, 2]) }`, ``},
		{`mutation { inc reset }`, ``},
		{`{ __schema { types { name fields { name type { kind ofType { name } } } } } }`, ``},
		{`{ nullableStrings scores: me { scores } }`, ``},
		{`{`, ``},
		{`{ me { id } }`, `{"unused": [1, {"a": null}]}`},
		{`query($v: Int) { echo(v: $v) }`, `{"v": 99999999999}`},
	}
	for _, sd := range seeds {
		f.Add(sd.query, sd.vars)
	}

	f.Fuzz(func(t *testing.T, query, vars string) {
		req := &Request{Query: query}
		if vars != "" {
			req.Variables = json.RawMessage(vars)
		}
		resp := e.Execute(context.Background(), req)
		out, err := resp.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("invalid response JSON: %s", out)
		}
		if resp.Data == nil && len(resp.Errors) == 0 {
			t.Fatalf("response without data or errors for %q", query)
		}
		resp.Release()
	})
}
