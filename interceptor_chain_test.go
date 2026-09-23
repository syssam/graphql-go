package graphql

import (
	"context"
	"strings"
	"testing"
)

// The variadic WithXInterceptor options are the usual way to register several
// interceptors. Chain exists for the other case: an extension package that
// exports one composed interceptor value. Request and Operation had it; Field
// and Subscription have the same shape and the same need.
func TestChainFieldInterceptorsRunsOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) FieldInterceptor {
		return FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			order = append(order, "->"+name)
			v, err := next(ctx)
			order = append(order, "<-"+name)
			return v, err
		})
	}
	_, e := newFixtureExecutor(t, WithFieldInterceptor(ChainFieldInterceptors(mk("a"), mk("b"))))
	run(t, e, `{ me { id } }`, "")
	got := strings.Join(order, " ")
	if !strings.HasPrefix(got, "->a ->b") || !strings.HasSuffix(got, "<-b <-a") {
		t.Fatalf("order = %q, want a outermost", got)
	}
}

func TestChainFieldInterceptorsEdgeCases(t *testing.T) {
	if got := ChainFieldInterceptors(); got == nil {
		t.Fatal("empty chain must be a usable no-op, not nil")
	}
	only := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		return next(ctx)
	})
	if got := ChainFieldInterceptors(only); got == nil {
		t.Fatal("single-element chain must not be nil")
	}
}

func TestChainSubscriptionInterceptorsRunsOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) SubscriptionInterceptor {
		return SubscriptionInterceptorFunc(func(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error) {
			order = append(order, name)
			return next(ctx, oc)
		})
	}
	chained := ChainSubscriptionInterceptors(mk("a"), mk("b"))
	if chained == nil {
		t.Fatal("chain is nil")
	}
	_, _ = chained.InterceptSubscription(context.Background(), nil,
		func(ctx context.Context, oc *OperationContext) (<-chan *Response, error) { return nil, nil })
	if got := strings.Join(order, " "); got != "a b" {
		t.Fatalf("order = %q, want \"a b\" (first is outermost)", got)
	}
	if ChainSubscriptionInterceptors() == nil {
		t.Fatal("empty chain must be a usable no-op, not nil")
	}
}

// The comment at the top of this file explains why Field and Subscription got
// tests: they were added last and matched an existing shape. The shape they
// matched -- Request and Operation -- never had one. All four are public API
// promising "composes interceptors so that the first is outermost", and an
// extension package exporting one composed value depends on that order: an
// interceptor that authenticates has to wrap the one that logs, not the other
// way round.
func TestChainRequestInterceptorsRunsOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) RequestInterceptor {
		return RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
			order = append(order, "->"+name)
			resp := next(ctx, req)
			order = append(order, "<-"+name)
			return resp
		})
	}
	_, e := newFixtureExecutor(t, WithRequestInterceptor(ChainRequestInterceptors(mk("a"), mk("b"), mk("c"))))
	run(t, e, `{ me { id } }`, "")
	if got, want := strings.Join(order, " "), "->a ->b ->c <-c <-b <-a"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

func TestChainOperationInterceptorsRunsOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) OperationInterceptor {
		return OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			order = append(order, "->"+name)
			resp := next(ctx, oc)
			order = append(order, "<-"+name)
			return resp
		})
	}
	_, e := newFixtureExecutor(t, WithOperationInterceptor(ChainOperationInterceptors(mk("a"), mk("b"), mk("c"))))
	run(t, e, `{ me { id } }`, "")
	if got, want := strings.Join(order, " "), "->a ->b ->c <-c <-b <-a"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

// "A nil or empty list is a no-op interceptor" -- and a no-op has to pass the
// request through, not merely be non-nil. Asserting non-nil, which is what the
// Field edge-case test does, would pass for a chain that swallowed every
// request and returned an empty response.
func TestChainWithNoInterceptorsPassesTheRequestThrough(t *testing.T) {
	_, e := newFixtureExecutor(t,
		WithRequestInterceptor(ChainRequestInterceptors()),
		WithOperationInterceptor(ChainOperationInterceptors()),
		WithFieldInterceptor(ChainFieldInterceptors()),
	)
	expectData(t, run(t, e, `{ me { id } }`, ""), `{"me":{"id":"1"}}`)
}

// And the one-element case returns that interceptor itself, so it must still
// be invoked rather than silently dropped.
func TestChainWithOneInterceptorStillRunsIt(t *testing.T) {
	var ran int
	_, e := newFixtureExecutor(t,
		WithRequestInterceptor(ChainRequestInterceptors(
			RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
				ran++
				return next(ctx, req)
			}))),
		WithOperationInterceptor(ChainOperationInterceptors(
			OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
				ran++
				return next(ctx, oc)
			}))),
	)
	expectData(t, run(t, e, `{ me { id } }`, ""), `{"me":{"id":"1"}}`)
	if ran != 2 {
		t.Errorf("a one-element chain ran %d of 2 interceptors", ran)
	}
}
