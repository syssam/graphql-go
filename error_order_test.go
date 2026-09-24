package graphql

import (
	"context"
	"strings"
	"testing"
)

// Errors are reported in document order. graphql-js, graphql-http (the
// GraphQL-over-HTTP specification's reference implementation), Apollo Server
// and graphql-yoga all do, and until this was added the same query answered
// the same errors in a different order run to run: fields here finish in
// whatever order their resolvers return, and 200 executions of a six-error
// query produced 150 distinct orderings.
//
// Every case runs repeatedly, because one run of a nondeterministic order
// passes about as often as not.
const errorOrderRuns = 50

func errorPaths(t *testing.T, e *Executor, query string) string {
	t.Helper()
	resp := e.Execute(context.Background(), &Request{Query: query})
	var b strings.Builder
	for i, err := range resp.Errors {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(err.Path.String())
	}
	return b.String()
}

func TestErrorsAreInDocumentOrder(t *testing.T) {
	_, e := newFixtureExecutor(t)

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{
			// Aliases of one field: nothing but the document says what the
			// order should be.
			name:  "sibling aliases",
			query: `{ c: fail b: fail a: fail }`,
			want:  "c b a",
		},
		{
			// The reverse of the above through the same fields, which a sort
			// on the response key rather than on document position would get
			// wrong in one of the two.
			name:  "sibling aliases reversed",
			query: `{ a: fail b: fail c: fail }`,
			want:  "a b c",
		},
		{
			name:  "nested objects keep the inner order",
			query: `{ z: me { fail failNullable } a: me { failNullable fail } }`,
			want:  "z.fail z.failNullable a.failNullable a.fail",
		},
		{
			name:  "list elements sort by index",
			query: `{ users(filter: {}) { fail } }`,
			want:  "users[0].fail users[1].fail users[2].fail",
		},
		{
			name:  "a deeper path sorts after its shallower sibling",
			query: `{ b: failNullable a: me { fail } }`,
			want:  "b a.fail",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range errorOrderRuns {
				if got := errorPaths(t, e, tc.query); got != tc.want {
					t.Fatalf("run %d: error paths = %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

// TestErrorsAreInDocumentOrderAcrossFragments covers the merged plan rather
// than the query text: a fragment's fields take their position from where the
// plan merged them, which is what the sort key records.
func TestErrorsAreInDocumentOrderAcrossFragments(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const query = `{ me { ...B a: fail ...A } }
fragment A on User { y: fail }
fragment B on User { x: fail }`
	want := errorPaths(t, e, query)
	if want == "" {
		t.Fatal("expected errors")
	}
	for i := range errorOrderRuns {
		if got := errorPaths(t, e, query); got != want {
			t.Fatalf("run %d: error paths = %q, want %q (order is not stable)", i, got, want)
		}
	}
	// Whatever order the plan merges them into, it is the order the response
	// keys appear in, which is what a client sees in data.
	if got, w := want, "me.x me.a me.y"; got != w {
		t.Errorf("error paths = %q, want %q", got, w)
	}
}

// TestErrorLimitNoticeStaysLast pins the one error that carries no sort key
// and must not be moved.
func TestErrorLimitNoticeStaysLast(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxErrors(2))
	resp := e.Execute(context.Background(), &Request{
		Query: `{ c: fail b: fail a: fail }`,
	})
	if len(resp.Errors) == 0 {
		t.Fatal("expected errors")
	}
	last := resp.Errors[len(resp.Errors)-1]
	if last.Extensions["code"] != CodeErrorLimitExceeded {
		t.Fatalf("last error = %v, want the %s notice", last, CodeErrorLimitExceeded)
	}
}
