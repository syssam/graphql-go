package graphql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecScalarsAndAliases(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ me { id n: name nick tags scores role } }`, "")
	expectData(t, resp, `{"me":{"id":"1","n":"Alice","nick":"al","tags":["a","b"],"scores":[1,null,3],"role":"ADMIN"}}`)
}

func TestExecNullableNull(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ user(id: "2") { nick bestFriend { name } } user2: user(id: "3") { bestFriend { name } } missing: user(id: "9") { id } }`, "")
	expectData(t, resp, `{"user":{"nick":null,"bestFriend":{"name":"Alice"}},"user2":{"bestFriend":null},"missing":null}`)
}

func TestExecNestedLists(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ me { friends { name friends { id } } } }`, "")
	expectData(t, resp, `{"me":{"friends":[{"name":"Bob","friends":[{"id":"1"}]},{"name":"Carol","friends":[]}]}}`)
}

func TestExecNonNullBubblesToNullableParent(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ user(id: "1") { id fail } }`, "")
	expectError(t, resp, `{"user":null}`, "user.fail", "boom")
}

func TestExecNonNullBubblesToRoot(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ me { id fail } }`, "")
	expectError(t, resp, `null`, "me.fail", "boom")
	if resp.HasRequestErrors() {
		t.Fatal("field errors with null data are not request errors")
	}
}

func TestExecNullableFieldError(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ me { id failNullable name } }`, "")
	expectError(t, resp, `{"me":{"id":"1","failNullable":null,"name":"Alice"}}`, "me.failNullable", "boom")
}

func TestExecNonNullOutOfRangeScalar(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ me { bigInt } }`, "")
	expectError(t, resp, `null`, "me.bigInt", "Int cannot represent non 32-bit signed integer value: 1099511627776")
}

func TestExecListElementNullability(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ nullableStrings strings }`, "")
	expectData(t, resp, `{"nullableStrings":["x",null],"strings":["x","y"]}`)

	resp = run(t, e, `{ maybeUsers { id } }`, "")
	expectData(t, resp, `{"maybeUsers":[{"id":"1"},null]}`)

	// friends is [User!]!: a failing non-null field inside an element nulls the
	// list, which is itself non-null, so the error bubbles to the user. Both
	// elements execute concurrently, so either or both may report the error.
	resp = run(t, e, `{ user(id: "1") { friends { id fail } } }`, "")
	if string(resp.Data) != `{"user":null}` || len(resp.Errors) == 0 || len(resp.Errors) > 2 {
		t.Fatalf("got %s %s", resp.Data, errorsJSON(resp.Errors))
	}
	for _, err := range resp.Errors {
		if p := err.Path.String(); p != "user.friends[0].fail" && p != "user.friends[1].fail" {
			t.Fatalf("unexpected error path %s", p)
		}
	}

	// maybeUsers is [User]: element failures null only the element.
	resp = run(t, e, `{ maybeUsers { id fail } }`, "")
	expectError(t, resp, `{"maybeUsers":[null,null]}`, "maybeUsers[0].fail", "boom")
}

func TestExecInterfaceAndTypename(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{
		__typename
		me { pet { __typename name ... on Dog { barks } ... on Cat { lives } } }
		bob: user(id: "2") { pet { name ...CatFields } }
		carol: user(id: "3") { pet { name } }
	}
	fragment CatFields on Cat { lives }`, "")
	expectData(t, resp, `{"__typename":"Query","me":{"pet":{"__typename":"Dog","name":"Rex","barks":true}},"bob":{"pet":{"name":"Tom","lives":9}},"carol":{"pet":null}}`)
}

func TestExecUnionAndNode(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{
		search(term: "x") { __typename ... on User { name } ... on Post { title author { id } } }
		node(id: "p1") { id ... on Post { title } }
		none: node(id: "zz") { id }
	}`, "")
	expectData(t, resp, `{"search":[{"__typename":"User","name":"Alice"},{"__typename":"Post","title":"Hello","author":{"id":"1"}}],"node":{"id":"p1","title":"Hello"},"none":null}`)
}

func TestExecFragmentMerging(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `query {
		me { ...A ...B friends { id } }
	}
	fragment A on User { id friends { name } }
	fragment B on User { name friends { tags } }`, "")
	expectData(t, resp, `{"me":{"id":"1","friends":[{"name":"Bob","tags":[],"id":"2"},{"name":"Carol","tags":["c"],"id":"3"}],"name":"Alice"}}`)
}

func TestExecArgumentsAndVariables(t *testing.T) {
	_, e := newFixtureExecutor(t)

	resp := run(t, e, `{ echo(v: 1, f: 2.5, b: true, list: 7) }`, "")
	expectData(t, resp, `{"echo":"{\"V\":1,\"S\":\"d\",\"F\":2.5,\"B\":true,\"List\":[7]}"}`)

	resp = run(t, e, `query($v: Int, $s: String = "vd", $list: [Int!]) { echo(v: $v, s: $s, list: $list) }`, `{"v": 3, "list": [1, 2]}`)
	expectData(t, resp, `{"echo":"{\"V\":3,\"S\":\"vd\",\"F\":null,\"B\":null,\"List\":[1,2]}"}`)

	resp = run(t, e, `query($s: String) { echo(s: $s) }`, `{"s": null}`)
	expectData(t, resp, `{"echo":"{\"V\":null,\"S\":null,\"F\":null,\"B\":null,\"List\":null}"}`)

	resp = run(t, e, `query($s: String) { echo(s: $s) }`, ``)
	expectData(t, resp, `{"echo":"{\"V\":null,\"S\":\"d\",\"F\":null,\"B\":null,\"List\":null}"}`)

	resp = run(t, e, `query($f: Filter) { users(filter: $f) { name } }`, `{"f": {"name": "Bob"}}`)
	expectData(t, resp, `{"users":[{"name":"Bob"}]}`)

	resp = run(t, e, `{ users(filter: {limit: 2}) { id } }`, "")
	expectData(t, resp, `{"users":[{"id":"1"},{"id":"2"}]}`)
}

func TestExecVariableErrors(t *testing.T) {
	_, e := newFixtureExecutor(t)
	cases := []struct {
		name, query, vars, want string
	}{
		{"missing required", `query($id: ID!) { user(id: $id) { id } }`, ``, `Variable "$id" of required type "ID!" was not provided.`},
		{"null required", `query($id: ID!) { user(id: $id) { id } }`, `{"id": null}`, `Variable "$id" of non-null type "ID!" must not be null.`},
		{"wrong scalar", `query($v: Int) { echo(v: $v) }`, `{"v": "x"}`, `got invalid value "x"`},
		{"bad enum", `query($f: Filter) { users(filter: $f) { id } }`, `{"f": {"limit": 1.5}}`, `non-integer`},
		{"unknown input field", `query($f: Filter) { users(filter: $f) { id } }`, `{"f": {"nope": 1}}`, `Field "nope" is not defined by type "Filter"`},
		{"malformed json", `{ me { id } }`, `[1]`, `variables must be a JSON object`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, e, tc.query, tc.vars)
			if !resp.HasRequestErrors() {
				t.Fatalf("expected request error, got data %s errors %s", resp.Data, errorsJSON(resp.Errors))
			}
			if !strings.Contains(resp.Errors[0].Message, tc.want) {
				t.Fatalf("message %q does not contain %q", resp.Errors[0].Message, tc.want)
			}
			if resp.Errors[0].Extensions["code"] != CodeBadUserInput {
				t.Fatalf("code = %v", resp.Errors[0].Extensions["code"])
			}
		})
	}
}

func TestExecSkipIncludeVariants(t *testing.T) {
	_, e := newFixtureExecutor(t)
	q := `query($a: Boolean!, $b: Boolean = false) { me { id name @include(if: $a) nick @skip(if: $b) ...F @include(if: $a) } } fragment F on User { tags }`

	resp := run(t, e, q, `{"a": true}`)
	expectData(t, resp, `{"me":{"id":"1","name":"Alice","nick":"al","tags":["a","b"]}}`)

	resp = run(t, e, q, `{"a": false, "b": true}`)
	expectData(t, resp, `{"me":{"id":"1"}}`)

	entry := e.cache.get(q)
	if entry == nil || len(entry.plans) != 2 {
		t.Fatalf("expected two cached plan variants, got %v", entry)
	}
	if e.cache.len() != 1 {
		t.Fatalf("expected one cached document, got %d", e.cache.len())
	}

	resp = run(t, e, `{ me { id @skip(if: true) name @include(if: false) nick } }`, "")
	expectData(t, resp, `{"me":{"nick":"al"}}`)
}

func TestExecMutationSerial(t *testing.T) {
	f, e := newFixtureExecutor(t)
	resp := run(t, e, `mutation { a: inc b: inc c: inc r: reset }`, "")
	expectData(t, resp, `{"a":1,"b":2,"c":3,"r":true}`)
	close(f.calls)
	var order []string
	for c := range f.calls {
		order = append(order, c)
	}
	if strings.Join(order, ",") != "inc1,inc2,inc3" {
		t.Fatalf("mutation fields ran out of order: %v", order)
	}
}

func TestExecQueryConcurrency(t *testing.T) {
	_, e := newFixtureExecutor(t)
	start := time.Now()
	resp := run(t, e, `{ a: slow(ms: 40) b: slow(ms: 40) c: slow(ms: 40) }`, "")
	expectData(t, resp, `{"a":40,"b":40,"c":40}`)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("resolvers did not run concurrently: %v", d)
	}

	_, serial := newFixtureExecutor(t, WithMaxConcurrency(0))
	start = time.Now()
	resp = run(t, serial, `{ a: slow(ms: 30) b: slow(ms: 30) }`, "")
	expectData(t, resp, `{"a":30,"b":30}`)
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("with concurrency disabled resolvers must run serially: %v", d)
	}
}

func TestExecConcurrencyInlineFallback(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxConcurrency(1))
	resp := run(t, e, `{ a: slow(ms: 5) b: slow(ms: 5) users { friends { id fail } } }`, "")
	if len(resp.Errors) == 0 || string(resp.Data) != `null` {
		t.Fatalf("expected bubbled failure, got %s %s", resp.Data, errorsJSON(resp.Errors))
	}
}

func TestExecConcurrentListElements(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ users { id friends { id bestFriend { id } } } }`, "")
	expectData(t, resp, `{"users":[{"id":"1","friends":[{"id":"2","bestFriend":{"id":"1"}},{"id":"3","bestFriend":null}]},{"id":"2","friends":[{"id":"1","bestFriend":{"id":"2"}}]},{"id":"3","friends":[]}]}`)
}

func TestExecPanicRecovery(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ panics me { id } }`, "")
	expectError(t, resp, `{"panics":null,"me":{"id":"1"}}`, "panics", "internal system error")
	if resp.Errors[0].Extensions["code"] != CodeInternal {
		t.Fatalf("code = %v", resp.Errors[0].Extensions["code"])
	}
}

func TestExecPanicPropagatesWhenRecoverDisabled(t *testing.T) {
	_, e := newFixtureExecutor(t, WithRecover(false))
	defer func() {
		if r := recover(); r != "kaboom" {
			t.Fatalf("expected panic to propagate, got %v", r)
		}
	}()
	run(t, e, `{ panics }`, "")
	t.Fatal("unreachable")
}

func TestExecOperationSelection(t *testing.T) {
	_, e := newFixtureExecutor(t)
	doc := `query A { me { id } } query B { me { name } }`

	resp := e.Execute(context.Background(), &Request{Query: doc, OperationName: "B"})
	expectData(t, resp, `{"me":{"name":"Alice"}}`)

	resp = e.Execute(context.Background(), &Request{Query: doc})
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeOperationResolution {
		t.Fatalf("expected operation resolution error, got %s", errorsJSON(resp.Errors))
	}

	resp = e.Execute(context.Background(), &Request{Query: doc, OperationName: "C"})
	if !resp.HasRequestErrors() || !strings.Contains(resp.Errors[0].Message, `"C"`) {
		t.Fatalf("expected unknown operation error, got %s", errorsJSON(resp.Errors))
	}
}

func TestExecParseAndValidationErrors(t *testing.T) {
	_, e := newFixtureExecutor(t)

	resp := run(t, e, `{ me { `, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeParseFailed || len(resp.Errors[0].Locations) == 0 {
		t.Fatalf("expected parse error with location, got %s", errorsJSON(resp.Errors))
	}

	resp = run(t, e, `{ me { nope } }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeValidationFailed {
		t.Fatalf("expected validation error, got %s", errorsJSON(resp.Errors))
	}
	if e.cache.len() != 0 {
		t.Fatal("invalid documents must not be cached")
	}
}

func TestExecContextCancellation(t *testing.T) {
	_, e := newFixtureExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := e.Execute(ctx, &Request{Query: `{ me { id name } }`})
	if len(resp.Errors) == 0 || resp.Errors[0].Extensions["code"] != CodeRequestCancelled {
		t.Fatalf("expected cancellation error, got %s (data %s)", errorsJSON(resp.Errors), resp.Data)
	}
	if string(resp.Data) != "null" {
		t.Fatalf("cancelled non-null root field must null the data, got %s", resp.Data)
	}
}

func TestExecErrorPresenterAndExtensions(t *testing.T) {
	masked := func(_ context.Context, err error) *Error {
		out := DefaultErrorPresenter(context.Background(), err)
		if errors.Is(err, errBoom) {
			out.Message = "masked"
			out.WithCode("MASKED")
		}
		return out
	}
	_, e := newFixtureExecutor(t, WithErrorPresenter(masked))
	resp := run(t, e, `{ failNullable }`, "")
	expectError(t, resp, `{"failNullable":null}`, "failNullable", "masked")
	if resp.Errors[0].Extensions["code"] != "MASKED" {
		t.Fatalf("extensions = %v", resp.Errors[0].Extensions)
	}
}

func TestExecResponseRelease(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for range 50 {
		resp := run(t, e, `{ me { id } }`, "")
		if string(resp.Data) != `{"me":{"id":"1"}}` {
			t.Fatalf("got %s", resp.Data)
		}
		resp.Release()
	}
}

func TestExecPathAndSelectionHelpers(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ p: ctxPath selection }`, "")
	expectData(t, resp, `{"p":"p","selection":[]}`)

	resp = run(t, e, `{ me { friends { id } } }`, "")
	expectData(t, resp, `{"me":{"friends":[{"id":"2"},{"id":"3"}]}}`)
}

// Executing a subscription is rejected by TestExecuteRejectsSubscription in
// subscription_test.go, against a schema whose subscription root is bound the
// way the schema builder now requires.

func TestExecIntrospectionDisabled(t *testing.T) {
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), append(f.options(), DisableIntrospection())...)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ __schema { types { name } } }`, "")
	if !resp.HasRequestErrors() || !strings.Contains(resp.Errors[0].Message, "introspection is not allowed") {
		t.Fatalf("expected introspection rejection, got %s", errorsJSON(resp.Errors))
	}
	resp = run(t, e, `{ __typename }`, "")
	expectData(t, resp, `{"__typename":"Query"}`)
}

func BenchmarkExecuteUsers(b *testing.B) {
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s)
	req := &Request{Query: `{ users { id name nick tags role } }`}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(ctx, req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

// BenchmarkExecuteConcurrentList covers the scheduled list path: a Resolve
// field beneath a list makes the selection deeply schedulable, so elements
// are written by concurrent tasks rather than inline. BenchmarkExecuteUsers
// does not reach it, because pure fields never schedule.
func BenchmarkExecuteConcurrentList(b *testing.B) {
	type row struct{ ID string }
	const n = 100

	rows := make([]*row, n)
	for i := range rows {
		rows[i] = &row{ID: itoa(int64(i))}
	}

	s, err := NewSchema(SDL(`
		type Row { id: ID! label: String! }
		type Query { rows: [Row!]! }
	`),
		Object[row]("Row",
			Field("id", func(r *row) ID { return ID(r.ID) }),
			Resolve("label", func(_ context.Context, r *row) (string, error) { return r.ID, nil }),
		),
		Query(Resolve("rows", func(context.Context, Root) ([]*row, error) { return rows, nil })),
	)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s)
	req := &Request{Query: `{ rows { id label } }`}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(ctx, req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

// TestExecOneOfInputObjects walks the coercion table in specification
// section 3.10.1. The literal rows are enforced by the validator; the
// variable rows are request errors raised while coercing variables.
func TestExecOneOfInputObjects(t *testing.T) {
	_, e := newFixtureExecutor(t)
	cases := []struct {
		name     string
		query    string
		vars     string
		wantData string
		wantErr  string
	}{
		{"literal single member", `{choose(c:{name:"abc"})}`, "", `{"choose":"name=abc"}`, ""},
		{"literal other member", `{choose(c:{score:123})}`, "", `{"choose":"score=123"}`, ""},
		{"variable single member", `query($v:Choice!){choose(c:$v)}`, `{"v":{"name":"abc"}}`, `{"choose":"name=abc"}`, ""},
		{"literal null member", `{choose(c:{name:null})}`, "", "", "must be non-null"},
		{"variable null member", `query($v:Choice!){choose(c:$v)}`, `{"v":{"name":null}}`, "", "must be non-null"},
		{"literal two members", `{choose(c:{name:"abc",score:123})}`, "", "", "exactly one key"},
		{"literal two members one null", `{choose(c:{name:"abc",score:null})}`, "", "", "exactly one key"},
		{"literal two members wrong types", `{choose(c:{name:456,score:"xyz"})}`, "", "", "exactly one key"},
		{"literal member plus absent variable", `query($s:Int){choose(c:{name:"abc",score:$s})}`, `{}`, "", "exactly one key"},
		{"literal no members", `{choose(c:{})}`, "", "", "exactly one key"},
		{"variable two members", `query($v:Choice!){choose(c:$v)}`, `{"v":{"name":"abc","score":123}}`, "", "exactly one key"},
		{"variable no members", `query($v:Choice!){choose(c:$v)}`, `{"v":{}}`, "", "exactly one key"},
		{"member from nullable variable", `query($n:String){choose(c:{name:$n})}`, `{}`, "", "OneOf"},
		{"two members from variables", `query($n:String,$s:Int){choose(c:{name:$n,score:$s})}`, `{"n":"abc"}`, "", "exactly one key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, e, tc.query, tc.vars)
			if tc.wantErr == "" {
				expectData(t, resp, tc.wantData)
				return
			}
			if got := string(resp.Data); got != tc.wantData {
				t.Fatalf("data = %s, want %q", got, tc.wantData)
			}
			if len(resp.Errors) == 0 {
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(errorsJSON(resp.Errors), tc.wantErr) {
				t.Fatalf("errors lack %q: %s", tc.wantErr, errorsJSON(resp.Errors))
			}
		})
	}
}

