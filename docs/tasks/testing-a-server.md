# Test your server

Test at three levels, smallest first, so that a failure says which layer broke: the
schema builds, a query executes, a request is served.

A server is three values, each built from the one before, and each is somewhere a test can
stand. The example serves notes that have replies; `NewNotesSchema` returns the first.

```go
func NewNotesExecutor(schema *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(schema,
		graphql.WithMaxDepth(4),
		graphql.WithOperationTimeout(5*time.Second),
	)
}

func NewNotesHandler(exec *graphql.Executor) http.Handler {
	return gqlhttp.New(exec)
}
```

## Level 1: the schema builds

`NewSchema` checks every bound Go function against the SDL and reports every mismatch, so
asserting that it returns no error covers fields no other test queries.

```go
func TestNotesSchemaBuilds(t *testing.T) {
	if _, err := NewNotesSchema(notesFixture()); err != nil {
		t.Fatal(err)
	}
}
```

Bind `Note.text` to a function that returns an `int` and this test fails with
`field Note.text: Go type int is not registered for String`.

## Level 2: a query executes

No HTTP. Build the executor the way the server does, hand it a `Request`, and assert on
`Data`, which is the JSON of the `data` member, and on `Errors`.

```go
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
```

An error a resolver returns is part of the answer. Assert on what a client can act on: the
path, the code and the message.

```go
e := resp.Errors[0]
if e.Path.String() != "note" || e.Extensions["code"] != "NOT_FOUND" || e.Message != `no note with id "404"` {
	t.Fatalf("error = %v at %q with code %v", e, e.Path, e.Extensions["code"])
}
```

The limits are configuration, and configuration nothing exercises goes missing. This
query is five levels deep against a limit of four.

```go
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
```

## Level 3: a request is served

One request through the real handler covers what the executor never sees: the route, the
content type, the status and the envelope.

```go
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
```

## Check it worked

```sh
go test -race -run Notes -v ./examples/howto/
```

## What goes wrong

- **A test fails one run in twenty, or only in CI.** Fields bound with `Resolve` run on
  goroutines of their own, so a fixture two of them touch is a data race. Run with `-race`
  every time.
- **`Data` is empty, or holds bytes of a different response.** It was read after
  `Release`. `Data` points into a pooled buffer that `Release` hands back and the next
  request overwrites. Defer `Release`, or copy with `string(resp.Data)` first.
- **The log says `this executor has no depth, complexity or cost limit`.** All three are
  off by default, and an executor built with none says so once per process. Set them where
  the server builds its executor, and keep a test that an oversized query is refused.
- **An HTTP test that asserts only the status passes for a query that failed.** A request
  posted as `application/json` is answered `200` with an `errors` member, a refused query
  included. Assert on the body.

## Next

- [`docs/operations.md`](../operations.md#before-it-faces-anything-untrusted): which limits
  are off by default and how to choose their values.
- [`examples/storefront/storefront_test.go`](../../examples/storefront/storefront_test.go):
  levels 1 and 2 on a larger schema, each query run as four different callers.
