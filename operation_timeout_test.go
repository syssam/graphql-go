package graphql

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// These tests run in a synctest bubble: every wait is on a deadline, so fake
// time makes them exact and instant instead of slow and flaky.

const timeoutSDL = `
type Tick { wait: Int }
type Query { wait: Int  after: String  waitWrapped: Int  waitCause: Int }
type Subscription { tick: Tick! }
`

type timeoutTick struct{ n int }

// timeoutFixture counts how often after resolves, which is how a test tells
// execution that stopped at the deadline from execution that carried on.
type timeoutFixture struct {
	afters atomic.Int64
	ticks  chan *timeoutTick
}

// waitForDeadline is a resolver that honours its context and nothing else,
// the shape a timeout can actually stop.
func waitForDeadline(ctx context.Context) (*int, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func newTimeoutExecutor(t *testing.T, opts ...ExecutorOption) (*timeoutFixture, *Executor) {
	t.Helper()
	f := &timeoutFixture{ticks: make(chan *timeoutTick)}
	s, err := NewSchema(SDL(timeoutSDL),
		Object[timeoutTick]("Tick",
			Resolve("wait", func(ctx context.Context, _ *timeoutTick) (*int, error) { return waitForDeadline(ctx) }),
		),
		Query(
			Resolve("wait", func(ctx context.Context, _ Root) (*int, error) { return waitForDeadline(ctx) }),
			// waitWrapped is a driver-style error: its own message and
			// extensions, wrapping the context error.
			Resolve("waitWrapped", func(ctx context.Context, _ Root) (*int, error) {
				<-ctx.Done()
				return nil, &Error{Message: "db: " + ctx.Err().Error(), Err: ctx.Err(), Extensions: map[string]any{"db": "primary"}}
			}),
			// waitCause reports why its context ended the Go 1.21 way.
			Resolve("waitCause", func(ctx context.Context, _ Root) (*int, error) {
				<-ctx.Done()
				return nil, context.Cause(ctx)
			}),
			Field("after", func(Root) *string {
				f.afters.Add(1)
				s := "after"
				return &s
			}),
		),
		Subscription(
			Subscribe("tick", func(context.Context) (<-chan *timeoutTick, error) { return f.ticks, nil }),
		),
	)
	if err != nil {
		t.Fatalf("timeout schema: %v", err)
	}
	return f, NewExecutor(s, opts...)
}

func errorCodes(errs []*Error) []any {
	out := make([]any, len(errs))
	for i, e := range errs {
		out[i] = e.Extensions["code"]
	}
	return out
}

// TestOperationTimeoutEndsResolver pins the ordinary case: a resolver waiting
// on its context is released at the timeout, and its error says why rather
// than surfacing as a bare "context deadline exceeded".
func TestOperationTimeoutEndsResolver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newTimeoutExecutor(t, WithOperationTimeout(50*time.Millisecond))
		start := time.Now()
		resp := run(t, e, `{ wait }`, "")
		if elapsed := time.Since(start); elapsed != 50*time.Millisecond {
			t.Fatalf("returned after %v, want exactly the 50ms timeout", elapsed)
		}
		if got := string(resp.Data); got != `{"wait":null}` {
			t.Fatalf("data = %s, want the field nulled", got)
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("want one error, got %s", errorsJSON(resp.Errors))
		}
		err := resp.Errors[0]
		if err.Extensions["code"] != CodeOperationTimeout {
			t.Errorf("code = %v, want %s", err.Extensions["code"], CodeOperationTimeout)
		}
		if err.Message != "operation exceeded its timeout of 50ms" {
			t.Errorf("message = %q", err.Message)
		}
		if err.Path.String() != "wait" {
			t.Errorf("path = %v, want wait", err.Path)
		}
	})
}

// TestOperationTimeoutStopsLaterFields covers fields that never see the
// deadline themselves: after is pure and takes no context, so it is stopped by
// the executor's checkpoint, which must also say timeout.
func TestOperationTimeoutStopsLaterFields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, e := newTimeoutExecutor(t, WithMaxConcurrency(0), WithOperationTimeout(50*time.Millisecond))
		resp := run(t, e, `{ wait after }`, "")
		if n := f.afters.Load(); n != 0 {
			t.Fatalf("after resolved %d times past the deadline", n)
		}
		if got := string(resp.Data); got != `{"wait":null,"after":null}` {
			t.Fatalf("data = %s", got)
		}
		codes := errorCodes(resp.Errors)
		if len(codes) != 2 || codes[0] != CodeOperationTimeout || codes[1] != CodeOperationTimeout {
			t.Fatalf("codes = %v, want two %s", codes, CodeOperationTimeout)
		}
	})
}

// TestOperationTimeoutLeavesCallerDeadlineAlone pins the distinction the new
// code exists for: a deadline the caller set is a cancellation, not the
// server's timeout, even when the executor also has one.
func TestOperationTimeoutLeavesCallerDeadlineAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newTimeoutExecutor(t, WithMaxConcurrency(0), WithOperationTimeout(time.Second))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		resp := e.Execute(ctx, &Request{Query: `{ wait after }`})
		for _, err := range resp.Errors {
			if err.Extensions["code"] == CodeOperationTimeout {
				t.Fatalf("the caller's own deadline was reported as the operation timeout: %s", errorsJSON(resp.Errors))
			}
		}
		if codes := errorCodes(resp.Errors); len(codes) != 2 || codes[1] != CodeRequestCancelled {
			t.Fatalf("codes = %v, want the checkpoint to report %s", codes, CodeRequestCancelled)
		}
	})
}

// TestOperationTimeoutUnset pins that no option means no deadline: the same
// query runs until the caller gives up.
func TestOperationTimeoutUnset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newTimeoutExecutor(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		start := time.Now()
		e.Execute(ctx, &Request{Query: `{ wait }`})
		if elapsed := time.Since(start); elapsed != time.Hour {
			t.Fatalf("returned after %v with no timeout configured, want the caller's hour", elapsed)
		}
	})
}

// TestOperationTimeoutPerSubscriptionEvent pins both halves of the
// subscription contract: each event is bounded, and the stream is not — it
// outlives many timeouts and keeps delivering.
func TestOperationTimeoutPerSubscriptionEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, e := newTimeoutExecutor(t, WithOperationTimeout(50*time.Millisecond))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out, err := e.Subscribe(ctx, &Request{Query: `subscription { tick { wait } }`})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}

		for i := range 2 {
			time.Sleep(time.Second) // far past the timeout between events
			start := time.Now()
			f.ticks <- &timeoutTick{n: i}
			resp := <-out
			if elapsed := time.Since(start); elapsed != 50*time.Millisecond {
				t.Fatalf("event %d took %v, want the 50ms timeout", i, elapsed)
			}
			if codes := errorCodes(resp.Errors); len(codes) != 1 || codes[0] != CodeOperationTimeout {
				t.Fatalf("event %d codes = %v, want one %s", i, codes, CodeOperationTimeout)
			}
			resp.Release()
		}
		close(f.ticks)
		if _, ok := <-out; ok {
			t.Fatal("stream did not close after its source did")
		}
	})
}

// TestOperationTimeoutKeepsResolverError pins that translating a timeout
// loses nothing the resolver said: a presenter can still find the context
// error in the chain, and the resolver's own extensions survive beside the code.
func TestOperationTimeoutKeepsResolverError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sawDeadline atomic.Bool
		presenter := func(ctx context.Context, err error) *Error {
			if errors.Is(err, context.DeadlineExceeded) {
				sawDeadline.Store(true)
			}
			return DefaultErrorPresenter(ctx, err)
		}
		_, e := newTimeoutExecutor(t, WithErrorPresenter(presenter), WithOperationTimeout(50*time.Millisecond))
		resp := run(t, e, `{ waitWrapped }`, "")
		if len(resp.Errors) != 1 {
			t.Fatalf("want one error, got %s", errorsJSON(resp.Errors))
		}
		ext := resp.Errors[0].Extensions
		if ext["code"] != CodeOperationTimeout || ext["db"] != "primary" {
			t.Fatalf("extensions = %v, want code %s and the resolver's db", ext, CodeOperationTimeout)
		}
		if !sawDeadline.Load() {
			t.Fatal("the presenter could not find context.DeadlineExceeded in the translated error's chain")
		}
	})
}

// TestOperationTimeoutReturnedCause covers a resolver returning
// context.Cause(ctx), which is the executor's own cause value rather than
// context.DeadlineExceeded.
func TestOperationTimeoutReturnedCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newTimeoutExecutor(t, WithOperationTimeout(50*time.Millisecond))
		resp := run(t, e, `{ waitCause }`, "")
		if codes := errorCodes(resp.Errors); len(codes) != 1 || codes[0] != CodeOperationTimeout {
			t.Fatalf("codes = %v, want one %s", codes, CodeOperationTimeout)
		}
	})
}

// TestOperationTimeoutCoversInterceptors pins where the deadline starts: at
// Execute, outside every request interceptor. Started any later, an
// interceptor doing I/O would be unbounded and this one would never return.
func TestOperationTimeoutCoversInterceptors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocking := RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
			<-ctx.Done()
			return next(ctx, req)
		})
		_, e := newTimeoutExecutor(t, WithRequestInterceptor(blocking), WithOperationTimeout(50*time.Millisecond))
		start := time.Now()
		resp := run(t, e, `{ after }`, "")
		if elapsed := time.Since(start); elapsed != 50*time.Millisecond {
			t.Fatalf("returned after %v, want the 50ms timeout", elapsed)
		}
		if codes := errorCodes(resp.Errors); len(codes) != 1 || codes[0] != CodeOperationTimeout {
			t.Fatalf("codes = %v, want one %s", codes, CodeOperationTimeout)
		}
	})
}
