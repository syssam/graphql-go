package graphql

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const errLimitSDL = `
type Row { bad: String }
type Query { rows: [Row!]! wait: Int }
`

type errLimitRow struct{ n int }

// errLimitFixture fails every Row.bad, and cancels the request when it
// reaches row cancelAt (if set), so a test can put an engine-level error
// behind a full error list.
type errLimitFixture struct {
	cancelAt int
	cancel   atomic.Pointer[context.CancelFunc]
}

func newErrLimitExecutor(t *testing.T, rows int, opts ...ExecutorOption) (*errLimitFixture, *Executor) {
	t.Helper()
	f := &errLimitFixture{cancelAt: -1}
	list := make([]*errLimitRow, rows)
	for i := range list {
		list[i] = &errLimitRow{n: i}
	}
	s, err := NewSchema(SDL(errLimitSDL),
		Object[errLimitRow]("Row",
			Resolve("bad", func(_ context.Context, r *errLimitRow) (*string, error) {
				if r.n == f.cancelAt {
					if c := f.cancel.Load(); c != nil {
						(*c)()
					}
				}
				return nil, fmt.Errorf("row %d failed", r.n)
			}),
		),
		Query(
			Field("rows", func(Root) []*errLimitRow { return list }),
			// wait reports its end only through its own field error, which is
			// what a full list could drop.
			Resolve("wait", func(ctx context.Context, _ Root) (*int, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}),
		),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return f, NewExecutor(s, opts...)
}

func expectErrorLimit(t *testing.T, errs []*Error, limit int) {
	t.Helper()
	if len(errs) != limit+1 {
		t.Fatalf("got %d errors, want %d kept plus the notice", len(errs), limit)
	}
	for i, e := range errs[:limit] {
		if e.Path == nil || e.Extensions["code"] == CodeErrorLimitExceeded {
			t.Fatalf("error %d = %+v, want a field error with a path", i, e)
		}
	}
	notice := errs[limit]
	if notice.Extensions["code"] != CodeErrorLimitExceeded || notice.Path != nil {
		t.Fatalf("last error = %+v, want the %s notice with no path", notice, CodeErrorLimitExceeded)
	}
	if notice.Message != "Too many errors: further errors were omitted." {
		t.Fatalf("notice message = %q", notice.Message)
	}
}

// TestMaxErrorsTruncatesFieldErrors: the data is untouched — every row still
// nulls its failed field — and only the errors list is cut.
func TestMaxErrorsTruncatesFieldErrors(t *testing.T) {
	_, e := newErrLimitExecutor(t, 50, WithMaxConcurrency(0), WithMaxErrors(10))
	resp := run(t, e, `{ rows { bad } }`, "")
	expectErrorLimit(t, resp.Errors, 10)
	if got := strings.Count(string(resp.Data), `{"bad":null}`); got != 50 {
		t.Fatalf("data has %d nulled rows, want all 50", got)
	}
}

// TestMaxErrorsIsExactUnderConcurrency: rows resolve on concurrent goroutines,
// and the limit still holds exactly rather than overshooting by however many
// raced past a check.
func TestMaxErrorsIsExactUnderConcurrency(t *testing.T) {
	_, e := newErrLimitExecutor(t, 200, WithMaxConcurrency(16), WithMaxErrors(10))
	for range 20 {
		resp := run(t, e, `{ rows { bad } }`, "")
		expectErrorLimit(t, resp.Errors, 10)
	}
}

// TestMaxErrorsSkipsWorkPastTheLimit is what bounds memory rather than just
// the output: past the limit an error is neither presented nor given a path.
func TestMaxErrorsSkipsWorkPastTheLimit(t *testing.T) {
	var presented atomic.Int32
	presenter := func(ctx context.Context, err error) *Error {
		presented.Add(1)
		return DefaultErrorPresenter(ctx, err)
	}
	_, e := newErrLimitExecutor(t, 50, WithMaxConcurrency(0), WithMaxErrors(10), WithErrorPresenter(presenter))
	run(t, e, `{ rows { bad } }`, "")
	if got := presented.Load(); got != 10 {
		t.Fatalf("presenter ran %d times for 50 errors under a limit of 10, want 10", got)
	}
}

// TestMaxErrorsKeepsEngineErrors: an error that says why the request stopped
// must reach the client even when field errors have already filled the list.
func TestMaxErrorsKeepsEngineErrors(t *testing.T) {
	f, e := newErrLimitExecutor(t, 50, WithMaxConcurrency(0), WithMaxErrors(5))
	f.cancelAt = 20
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.cancel.Store(&cancel)

	resp := e.Execute(ctx, &Request{Query: `{ rows { bad } }`})
	var sawCancel, sawNotice bool
	for _, err := range resp.Errors {
		switch err.Extensions["code"] {
		case CodeRequestCancelled:
			sawCancel = true
		case CodeErrorLimitExceeded:
			sawNotice = true
		}
	}
	if !sawCancel || !sawNotice {
		t.Fatalf("errors %s: want both the cancellation and the limit notice", errorsJSON(resp.Errors))
	}
}

// TestMaxErrorsTruncatesRequestErrors: validation of one large invalid query
// can produce an error per field, bounded only by the body size.
func TestMaxErrorsTruncatesRequestErrors(t *testing.T) {
	_, e := newErrLimitExecutor(t, 1, WithMaxErrors(5))
	var q strings.Builder
	q.WriteString("{ ")
	for i := range 30 {
		fmt.Fprintf(&q, "f%d: nope%d ", i, i)
	}
	q.WriteString("}")
	resp := run(t, e, q.String(), "")
	if len(resp.Errors) != 6 {
		t.Fatalf("got %d request errors, want 5 plus the notice", len(resp.Errors))
	}
	if code := resp.Errors[5].Extensions["code"]; code != CodeErrorLimitExceeded {
		t.Fatalf("last request error code = %v, want %s", code, CodeErrorLimitExceeded)
	}
}

func TestMaxErrorsZeroIsUnlimited(t *testing.T) {
	_, e := newErrLimitExecutor(t, 50, WithMaxConcurrency(0), WithMaxErrors(0))
	if got := len(run(t, e, `{ rows { bad } }`, "").Errors); got != 50 {
		t.Fatalf("got %d errors with no limit, want 50", got)
	}
}

func TestMaxErrorsDefault(t *testing.T) {
	_, e := newErrLimitExecutor(t, 1)
	if e.maxErrors != 1000 {
		t.Fatalf("default maxErrors = %d, want 1000", e.maxErrors)
	}
}

// TestMaxErrorsKeepsWhyTheRequestStopped: a field that ends with its context
// may be the only thing that reports the timeout or cancellation, and by then
// field errors can have filled the list. Dropping it must still leave the
// engine's error saying why the request stopped.
func TestMaxErrorsKeepsWhyTheRequestStopped(t *testing.T) {
	cases := []struct {
		name string
		opts []ExecutorOption
		ctx  func() (context.Context, context.CancelFunc)
		want string
	}{
		{
			name: "operation timeout",
			opts: []ExecutorOption{WithOperationTimeout(50 * time.Millisecond)},
			ctx:  func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			want: CodeOperationTimeout,
		},
		{
			name: "caller deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 50*time.Millisecond)
			},
			want: CodeRequestCancelled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				opts := append([]ExecutorOption{WithMaxConcurrency(0), WithMaxErrors(5)}, tc.opts...)
				_, e := newErrLimitExecutor(t, 50, opts...)
				ctx, cancel := tc.ctx()
				defer cancel()
				resp := e.Execute(ctx, &Request{Query: `{ rows { bad } wait }`})
				for _, err := range resp.Errors {
					if err.Extensions["code"] == tc.want {
						return
					}
				}
				t.Fatalf("errors %s: want %s to survive a full list", errorsJSON(resp.Errors), tc.want)
			})
		})
	}
}
