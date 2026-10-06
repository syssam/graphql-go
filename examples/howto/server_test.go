package howto

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
)

func notesFixture() []*Note {
	return []*Note{
		{ID: "1", Text: "Ship it", Replies: []*Note{{ID: "2", Text: "After the tests pass"}}},
		{ID: "3", Text: "Lunch?"},
	}
}

// Level 1. Every Go function bound to the schema is checked against the SDL
// when the schema is built, so this one assertion covers every field, whether
// or not another test queries it.
func TestNotesSchemaBuilds(t *testing.T) {
	if _, err := NewNotesSchema(notesFixture()); err != nil {
		t.Fatal(err)
	}
}

// What level 1 is worth: the same schema with Note.text bound to a function
// that returns an int does not build, and the error names the field.
func TestNotesSchemaRefusesAMismatchedBinding(t *testing.T) {
	wrong := graphql.Object[Note]("Note",
		graphql.Field("id", func(n *Note) graphql.ID { return graphql.ID(n.ID) }),
		graphql.Field("text", func(n *Note) int { return len(n.Text) }),
		graphql.Field("replies", func(n *Note) []*Note { return n.Replies }),
	)
	// The Query bindings are kept and the Note bindings replaced.
	_, err := graphql.NewSchema(graphql.SDL(notesSDL), wrong, notesBindings(nil)[1])
	if err == nil {
		t.Fatal("a String! field bound to a func returning int built")
	}
	if want := "field Note.text: Go type int is not registered for String"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v\nwant one containing %q", err, want)
	}
}

func notesExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	schema, err := NewNotesSchema(notesFixture())
	if err != nil {
		t.Fatal(err)
	}
	return NewNotesExecutor(schema)
}

// Level 2. No HTTP: a request in, a response out.
func TestNotesQuery(t *testing.T) {
	exec := notesExecutor(t)

	resp := exec.Execute(context.Background(), &graphql.Request{
		Query: `{ notes { text replies { text } } }`,
	})
	// Release hands the response's buffer back to a pool, and Data points
	// into that buffer. Deferred, so every read below happens first.
	defer resp.Release()

	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}
	want := `{"notes":[{"text":"Ship it","replies":[{"text":"After the tests pass"}]},{"text":"Lunch?","replies":[]}]}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data = %s\nwant   %s", got, want)
	}
}

// An error a resolver returns is part of the answer: assert on what a client
// can act on, which is the path, the code and the message.
func TestNotesQueryReportsAMissingNote(t *testing.T) {
	exec := notesExecutor(t)

	resp := exec.Execute(context.Background(), &graphql.Request{
		Query:     `query($id: ID!) { note(id: $id) { text } }`,
		Variables: []byte(`{"id": "404"}`),
	})
	defer resp.Release()

	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", resp.Errors)
	}
	e := resp.Errors[0]
	if e.Path.String() != "note" || e.Extensions["code"] != "NOT_FOUND" || e.Message != `no note with id "404"` {
		t.Fatalf("error = %v at %q with code %v", e, e.Path, e.Extensions["code"])
	}
	// Query.note is non-null, so the error takes the whole data with it.
	if got := string(resp.Data); got != "null" {
		t.Fatalf("data = %s, want null", got)
	}
}

// The limit is configuration, and configuration that is never exercised is
// the kind that goes missing. Five levels against a limit of four.
func TestNotesRefusesAQueryThatIsTooDeep(t *testing.T) {
	resp := notesExecutor(t).Execute(context.Background(), &graphql.Request{
		Query: `{ notes { replies { replies { replies { text } } } } }`,
	})
	defer resp.Release()

	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != graphql.CodeMaxDepth {
		t.Fatalf("errors = %v, want one %s", resp.Errors, graphql.CodeMaxDepth)
	}
	if resp.Data != nil {
		t.Fatalf("a refused query still ran: %s", resp.Data)
	}
}

// Level 3. One request through the real handler, to cover what the executor
// never sees: the route, the content type, the status, the envelope.
func TestNotesOverHTTP(t *testing.T) {
	srv := httptest.NewServer(NewNotesHandler(notesExecutor(t)))
	defer srv.Close()

	res, err := srv.Client().Post(srv.URL, "application/json",
		strings.NewReader(`{"query": "{ note(id: \"3\") { text } }"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.StatusCode, body)
	}
	if got, want := string(body), `{"data":{"note":{"text":"Lunch?"}}}`; got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}
