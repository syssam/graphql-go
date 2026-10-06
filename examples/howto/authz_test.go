package howto

import (
	"context"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
)

func billingExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	schema, err := NewBillingSchema([]*Invoice{
		{ID: "1", Owner: "ada", Total: 1200, Note: "paid late"},
		{ID: "2", Owner: "grace", Total: 800, Note: "disputed"},
		{ID: "3", Owner: "ada", Total: 300, Note: "first order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewBillingExecutor(schema)
}

var (
	anonymous = Caller{}
	ada       = Caller{Name: "ada", Scopes: map[string]bool{"user": true}}
	staff     = Caller{Name: "sam", Scopes: map[string]bool{"user": true, "staff": true}}
)

// ask runs query as caller and returns the data and the errors. The data is
// copied out because the response's buffer goes back to a pool.
func ask(t *testing.T, e *graphql.Executor, caller Caller, query string) (string, []*graphql.Error) {
	t.Helper()
	resp := e.Execute(WithCaller(context.Background(), caller), &graphql.Request{Query: query})
	defer resp.Release()
	return string(resp.Data), resp.Errors
}

func TestAnonymousCallerIsRefused(t *testing.T) {
	data, errs := ask(t, billingExecutor(t), anonymous, `{ invoices { id total } }`)

	if len(errs) != 1 || errs[0].Extensions["code"] != graphql.CodeForbidden {
		t.Fatalf("errors = %v, want one %s", errs, graphql.CodeForbidden)
	}
	// Query.invoices is non-null, so refusing it leaves no data at all.
	if data != "null" {
		t.Fatalf("a refused request still carried data: %s", data)
	}
}

func TestUserIsServedTheirOwnInvoices(t *testing.T) {
	e := billingExecutor(t)
	data, errs := ask(t, e, ada, `{ invoices { id total note } }`)

	if len(errs) > 0 {
		t.Fatalf("unexpected error: %v", errs[0])
	}
	// Invoice 2 is Grace's, so it is dropped from the list. The note is
	// staff-only, so it is null on the rows that remain.
	want := `{"invoices":[{"id":"1","total":1200,"note":null},{"id":"3","total":300,"note":null}]}`
	if data != want {
		t.Fatalf("data = %s\nwant   %s", data, want)
	}
}

// At a single position there is no list to drop a row from, so the row is
// null: someone else's invoice answers exactly as an id that does not exist.
func TestSomeoneElsesInvoiceLooksLikeAMissingOne(t *testing.T) {
	e := billingExecutor(t)
	for _, id := range []string{"2", "404"} {
		data, errs := ask(t, e, ada, `{ invoice(id: "`+id+`") { total } }`)
		if len(errs) > 0 || data != `{"invoice":null}` {
			t.Fatalf("invoice %s: data = %s, errors = %v", id, data, errs)
		}
	}
}

func TestStaffIsServedEverything(t *testing.T) {
	e := billingExecutor(t)
	data, errs := ask(t, e, staff, `{ invoices { id note } }`)

	if len(errs) > 0 {
		t.Fatalf("unexpected error: %v", errs[0])
	}
	want := `{"invoices":[{"id":"1","note":"paid late"},{"id":"2","note":"disputed"},{"id":"3","note":"first order"}]}`
	if data != want {
		t.Fatalf("data = %s\nwant   %s", data, want)
	}
}

// The directives describe positions and the executor options enforce them.
// An executor built without the options serves every position to anyone,
// which is why the page tells the reader to test the refusal and not the
// schema.
func TestNothingIsEnforcedWithoutTheExecutorOptions(t *testing.T) {
	schema, err := NewBillingSchema([]*Invoice{{ID: "1", Owner: "ada", Total: 1200, Note: "paid late"}})
	if err != nil {
		t.Fatal(err)
	}
	data, errs := ask(t, graphql.NewExecutor(schema), anonymous, `{ invoices { total note } }`)
	if len(errs) > 0 || data != `{"invoices":[{"total":1200,"note":"paid late"}]}` {
		t.Fatalf("data = %s, errors = %v", data, errs)
	}
}

// buildError builds a one-field schema around sdl and returns what NewSchema
// said about it.
func buildError(t *testing.T, sdl string) string {
	t.Helper()
	_, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.RequirementDirective("auth", "requires", graphql.ScopesAllOf),
		graphql.Query(graphql.FieldArgs("secret", func(graphql.Root, struct{ Hint *string }) string { return "" })),
	)
	if err == nil {
		t.Fatal("the schema built")
	}
	return err.Error()
}

// A requirement is compiled into the plan of each query, and only an object,
// an interface or a field is a position a plan has. Anywhere else the
// directive could never run, so the build refuses it rather than leave an
// author believing something is guarded.
func TestRequirementWhereItCannotRunIsABuildError(t *testing.T) {
	got := buildError(t, `
directive @auth(requires: [String!]) on FIELD_DEFINITION | ARGUMENT_DEFINITION
type Query { secret(hint: String @auth(requires: ["staff"])): String! }
`)
	want := `Query.secret(hint:): @auth is not enforced on an argument; declare it on an object, interface or field`
	if !strings.Contains(got, want) {
		t.Fatalf("error = %s\nwant one containing %s", got, want)
	}
}

// An empty list would be a requirement everyone satisfies, which reads as
// guarded and guards nothing.
func TestEmptyRequirementIsABuildError(t *testing.T) {
	got := buildError(t, `
directive @auth(requires: [String!]) on FIELD_DEFINITION
type Query { secret(hint: String): String! @auth(requires: []) }
`)
	want := `Query.secret: @auth requires must be a non-empty list of strings`
	if !strings.Contains(got, want) {
		t.Fatalf("error = %s\nwant one containing %s", got, want)
	}
}
