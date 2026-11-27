package graphql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

type dbCodedErr struct{ ext map[string]any }

func (e dbCodedErr) Error() string                     { return "pq: relation \"users\" does not exist" }
func (e dbCodedErr) GraphQLExtensions() map[string]any { return e.ext }

// An error from a driver carries SQL, table names or addresses in its text, and the
// default presenter writes that text to the client. The mask replaces it, keeps the
// error for the server's logs, and says INTERNAL_SERVER_ERROR as the engine does for a
// panic.
func TestMaskingErrorPresenterHidesWhatTheDefaultLeaks(t *testing.T) {
	t.Parallel()
	raw := fmt.Errorf("load user 7: %w", errors.New(`pq: relation "users" does not exist`))

	if got := DefaultErrorPresenter(context.Background(), raw).Message; !strings.Contains(got, "relation") {
		t.Fatalf("the premise changed: the default presenter no longer passes the text through (%q)", got)
	}

	out := MaskingErrorPresenter(context.Background(), raw)
	if out.Message != "internal system error" {
		t.Errorf("Message = %q, want the mask", out.Message)
	}
	if out.Extensions["code"] != CodeInternal {
		t.Errorf("code = %v, want %s", out.Extensions["code"], CodeInternal)
	}
	if !errors.Is(out, raw) && !errors.Is(out.Err, raw) {
		t.Errorf("Err = %v, want the original kept for logging", out.Err)
	}
	if body := errorsJSON([]*Error{out}); strings.Contains(body, "relation") || strings.Contains(body, "load user") {
		t.Errorf("the response still carries the original text: %s", body)
	}
}

// What an author wrote for the client is not an internal failure, and masking it would
// leave every "not found" and every validation message reading "internal system error".
func TestMaskingErrorPresenterKeepsErrorsWrittenForTheClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, err := range map[string]error{
		"an *Error":              Errorf("no such order").WithCode("NOT_FOUND"),
		"an *Error wrapped":      fmt.Errorf("get order: %w", Errorf("no such order").WithCode("NOT_FOUND")),
		"a validator's error":    &gqlerror.Error{Message: "Cannot query field \"x\" on type \"Query\"."},
		"a parser's error, wrap": fmt.Errorf("parse: %w", &gqlerror.Error{Message: "Unexpected Name"}),
	} {
		want := DefaultErrorPresenter(ctx, err)
		got := MaskingErrorPresenter(ctx, err)
		if got.Message != want.Message || fmt.Sprint(got.Extensions) != fmt.Sprint(want.Extensions) {
			t.Errorf("%s: masked to %q %v, want %q %v", name, got.Message, got.Extensions, want.Message, want.Extensions)
		}
		if got.Message == "internal system error" {
			t.Errorf("%s: was masked", name)
		}
	}
}

// An ExtensionsProvider is the app saying what the client may learn: its code wins over
// the mask's, and its other entries survive, while the message is still hidden.
func TestMaskingErrorPresenterHonoursExtensionsOfAnUnrecognisedError(t *testing.T) {
	t.Parallel()
	out := MaskingErrorPresenter(context.Background(), dbCodedErr{ext: map[string]any{"code": "CONFLICT", "retry": true}})
	if out.Message != "internal system error" {
		t.Errorf("Message = %q, want the mask", out.Message)
	}
	if out.Extensions["code"] != "CONFLICT" || out.Extensions["retry"] != true {
		t.Errorf("extensions = %v, want the provider's code and retry kept", out.Extensions)
	}

	out = MaskingErrorPresenter(context.Background(), dbCodedErr{ext: map[string]any{"retry": true}})
	if out.Extensions["code"] != CodeInternal || out.Extensions["retry"] != true {
		t.Errorf("extensions = %v, want INTERNAL_SERVER_ERROR beside retry", out.Extensions)
	}
}

// Through the executor, so a presenter nothing calls cannot pass: a resolver's failure
// reaches the client as the mask, and the default still shows it.
func TestMaskingErrorPresenterThroughTheExecutor(t *testing.T) {
	t.Parallel()
	_, plain := newFixtureExecutor(t)
	_, masked := newFixtureExecutor(t, WithErrorPresenter(MaskingErrorPresenter))

	if resp := run(t, plain, `{ fail }`, ""); len(resp.Errors) != 1 || resp.Errors[0].Message != "boom" {
		t.Fatalf("premise: default presenter errors = %s", errorsJSON(resp.Errors))
	}
	resp := run(t, masked, `{ fail }`, "")
	if len(resp.Errors) != 1 || resp.Errors[0].Message != "internal system error" || resp.Errors[0].Extensions["code"] != CodeInternal {
		t.Fatalf("masked errors = %s", errorsJSON(resp.Errors))
	}
	if len(resp.Errors[0].Path) == 0 {
		t.Errorf("the mask dropped the path: %s", errorsJSON(resp.Errors))
	}
}

// The mask is for what the server's own code returned. Everything the engine says about
// the request -- it did not parse, a variable did not coerce, a limit was passed -- is an
// error the client can act on, and a mask that reached it would leave a client unable to
// tell its own mistake from an outage. Each is presented exactly as the default does.
func TestMaskingErrorPresenterLeavesRequestErrorsAlone(t *testing.T) {
	t.Parallel()
	opts := []ExecutorOption{WithMaxDepth(3), WithMaxComplexity(12), WithQueryCost(QueryCost{Max: 50}), WithOperationTimeout(20 * time.Millisecond)}
	_, plain := newFixtureExecutor(t, opts...)
	_, masked := newFixtureExecutor(t, append([]ExecutorOption{WithErrorPresenter(MaskingErrorPresenter)}, opts...)...)
	for name, tc := range map[string]struct{ query, vars string }{
		"a parse error":                   {`{ me {`, ""},
		"an unknown field":                {`{ nope }`, ""},
		"a missing required argument":     {`{ user }`, ""},
		"a missing required variable":     {`query($id: ID!){ user(id:$id){name} }`, `{}`},
		"a null for a non-null variable":  {`query($id: ID!){ user(id:$id){name} }`, `{"id":null}`},
		"a variable of the wrong type":    {`query($id: ID!){ user(id:$id){name} }`, `{"id":[1]}`},
		"variables that are not JSON":     {`query($id: ID!){ user(id:$id){name} }`, `{"id":`},
		"an unknown input field":          {`query($f: Filter){ users(filter:$f){name} }`, `{"f":{"zzz":1}}`},
		"an Int out of range":             {`query($f: Filter){ users(filter:$f){name} }`, `{"f":{"limit":99999999999999}}`},
		"a @oneOf input with two members": {`query($c: Choice!){ choose(c:$c) }`, `{"c":{"name":"a","score":1}}`},
		"a bad literal":                   {`{ users(filter:{limit:"x"}){name} }`, ""},
		"the depth limit":                 {`{ me { friends { friends { friends { name }}}}}`, ""},
		"the cost limit":                  {`{ a: users { friends { name } } b: users { friends { name } } c: users{friends{name}} }`, ""},
		"the operation timeout":           {`{ slow(ms: 2000) }`, ""},
	} {
		want := run(t, plain, tc.query, tc.vars)
		got := run(t, masked, tc.query, tc.vars)
		if len(want.Errors) == 0 {
			t.Errorf("%s: premise: the default presenter reported no error", name)
			continue
		}
		if w, g := errorsJSON(want.Errors), errorsJSON(got.Errors); w != g {
			t.Errorf("%s: the mask changed what the client is told\n default: %s\n masked : %s", name, w, g)
		}
	}
}

// What the engine fails to write is the server's fault too: a resolver that returns an
// int64 for an Int says so with the value, and the client can do nothing with it.
func TestMaskingErrorPresenterHidesAnOutputCoercionFailure(t *testing.T) {
	t.Parallel()
	_, plain := newFixtureExecutor(t)
	_, masked := newFixtureExecutor(t, WithErrorPresenter(MaskingErrorPresenter))
	const q = `{ me { bigInt } }`
	if w := errorsJSON(run(t, plain, q, "").Errors); !strings.Contains(w, "1099511627776") {
		t.Fatalf("premise: the default no longer reports the value: %s", w)
	}
	if g := errorsJSON(run(t, masked, q, "").Errors); strings.Contains(g, "1099511627776") || !strings.Contains(g, "internal system error") {
		t.Errorf("masked: %s", g)
	}
}
