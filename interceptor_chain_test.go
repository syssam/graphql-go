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
