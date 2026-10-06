package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/syssam/graphql-go/internal/jsonw"
)

func TestResponseWriteTo(t *testing.T) {
	cases := []struct {
		name string
		resp Response
		want string
	}{
		{"data only", Response{Data: []byte(`{"a":1}`)}, `{"data":{"a":1}}`},
		{"errors only", Response{Errors: []*Error{Errorf("e")}}, `{"errors":[{"message":"e"}]}`},
		{"both", Response{Data: []byte(`null`), Errors: []*Error{Errorf("e")}}, `{"errors":[{"message":"e"}],"data":null}`},
		{"extensions", Response{Data: []byte(`{}`), Extensions: map[string]any{"t": 1}}, `{"data":{},"extensions":{"t":1}}`},
		{"empty", Response{}, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := tc.resp.WriteTo(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if int(n) != buf.Len() {
				t.Fatalf("n = %d, want %d", n, buf.Len())
			}
			if !json.Valid(buf.Bytes()) {
				t.Fatal("invalid JSON")
			}
		})
	}
}

func TestResponseHasRequestErrors(t *testing.T) {
	if (&Response{Errors: []*Error{Errorf("e")}}).HasRequestErrors() != true {
		t.Fatal("errors without data must be request errors")
	}
	if (&Response{Data: []byte("null"), Errors: []*Error{Errorf("e")}}).HasRequestErrors() {
		t.Fatal("errors with data are field errors")
	}
}

func TestResponseReleaseIdempotent(t *testing.T) {
	w := jsonw.Get()
	w.Null()
	r := &Response{Data: w.Bytes(), buf: w}
	r.Release()
	if r.Data != nil || r.buf != nil {
		t.Fatal("Release must clear Data and buffer")
	}
	r.Release()
}

func TestRequestJSON(t *testing.T) {
	var req Request
	err := json.Unmarshal([]byte(`{"query":"{a}","operationName":"Q","variables":{"x":1},"extensions":{"p":true}}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Query != "{a}" || req.OperationName != "Q" || string(req.Variables) != `{"x":1}` || req.Extensions["p"] != true {
		t.Fatalf("unexpected: %+v", req)
	}
}

// QueryOnly is enforced on the operation the executor is about to run, which
// is the only place the final operation is known: a RequestInterceptor may
// replace the query text, so a transport that checked the text it received
// has not checked the text that runs.
func TestQueryOnlyRefusesAMutation(t *testing.T) {
	refused := func(t *testing.T, resp *Response) {
		t.Helper()
		if !resp.HasRequestErrors() || len(resp.Errors) != 1 {
			t.Fatalf("response = %s data=%s, want one error and no data", errorsJSON(resp.Errors), resp.Data)
		}
		if got := resp.Errors[0].Extensions["code"]; got != CodeQueryOnly {
			t.Fatalf("code = %v, want %s", got, CodeQueryOnly)
		}
	}

	t.Run("a mutation is refused and does not run", func(t *testing.T) {
		f, e := newFixtureExecutor(t)
		refused(t, e.Execute(context.Background(), &Request{Query: `mutation { inc }`, QueryOnly: true}))
		if n := f.counter.Load(); n != 0 {
			t.Fatalf("the mutation ran %d time(s)", n)
		}
	})

	t.Run("a query runs", func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		expectData(t, e.Execute(context.Background(), &Request{Query: `{ __typename }`, QueryOnly: true}), `{"__typename":"Query"}`)
	})

	t.Run("without the flag a mutation runs", func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		expectData(t, e.Execute(context.Background(), &Request{Query: `mutation { inc }`}), `{"inc":1}`)
	})

	t.Run("an interceptor that hands on a request of its own does not lose it", func(t *testing.T) {
		f, e := newFixtureExecutor(t, WithRequestInterceptor(RequestInterceptorFunc(
			func(ctx context.Context, req *Request, next RequestHandler) *Response {
				return next(ctx, &Request{Query: `mutation { inc }`, Variables: req.Variables})
			})))
		refused(t, e.Execute(context.Background(), &Request{Query: `{ __typename }`, QueryOnly: true}))
		if n := f.counter.Load(); n != 0 {
			t.Fatalf("the mutation ran %d time(s)", n)
		}
		// The same interceptor on a request that may run one.
		expectData(t, e.Execute(context.Background(), &Request{Query: `{ __typename }`}), `{"inc":1}`)
	})

	t.Run("the operation is the one an interceptor left, not the one sent", func(t *testing.T) {
		f, e := newFixtureExecutor(t, WithRequestInterceptor(RequestInterceptorFunc(
			func(ctx context.Context, req *Request, next RequestHandler) *Response {
				req.Query = `mutation { inc }`
				return next(ctx, req)
			})))
		refused(t, e.Execute(context.Background(), &Request{Query: `{ __typename }`, QueryOnly: true}))
		if n := f.counter.Load(); n != 0 {
			t.Fatalf("the mutation ran %d time(s)", n)
		}
	})

	t.Run("it is selected by operation name like any other", func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		const doc = `query Q { __typename } mutation M { inc }`
		expectData(t, e.Execute(context.Background(), &Request{Query: doc, OperationName: "Q", QueryOnly: true}), `{"__typename":"Query"}`)
		refused(t, e.Execute(context.Background(), &Request{Query: doc, OperationName: "M", QueryOnly: true}))
	})

	t.Run("no operation interceptor sees a refused operation", func(t *testing.T) {
		ran := 0
		_, e := newFixtureExecutor(t, WithOperationInterceptor(OperationInterceptorFunc(
			func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
				ran++
				return next(ctx, oc)
			})))
		refused(t, e.Execute(context.Background(), &Request{Query: `mutation { inc }`, QueryOnly: true}))
		if ran != 0 {
			t.Fatalf("an operation interceptor ran %d time(s) for a refused mutation", ran)
		}
	})

	// A transport turns the refusal into its own status by finding the code,
	// so the code has to survive a presenter that rewrites every error.
	t.Run("a masking presenter leaves the refusal recognisable", func(t *testing.T) {
		_, e := newFixtureExecutor(t, WithErrorPresenter(func(context.Context, error) *Error {
			return Errorf("masked")
		}))
		refused(t, e.Execute(context.Background(), &Request{Query: `mutation { inc }`, QueryOnly: true}))
	})

	t.Run("the flag cannot be set from a request body", func(t *testing.T) {
		var req Request
		if err := json.Unmarshal([]byte(`{"query":"{ __typename }","QueryOnly":true,"queryOnly":true}`), &req); err != nil {
			t.Fatal(err)
		}
		if req.QueryOnly {
			t.Fatal("QueryOnly was decoded from JSON")
		}
	})
}

// Subscribe runs subscriptions only, so the flag has nothing to refuse there:
// graphql-sse opens a subscription with a GET.
func TestQueryOnlyDoesNotRefuseASubscription(t *testing.T) {
	_, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.Subscribe(ctx, &Request{Query: `subscription { counter }`, QueryOnly: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
}
