package throttle_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/throttle"
)

type item struct{ ID graphql.ID }
type pageArgs struct{ First *int }
type shopKey struct{}

// Each item costs one point, so items(first: n) costs 1 + n and a test can
// name the price it expects.
func newExec(t *testing.T, opts ...graphql.ExecutorOption) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`
		type Item { id: ID! }
		type Query { items(first: Int): [Item!]! }
	`),
		graphql.Object[item]("Item", graphql.Field("id", func(i *item) graphql.ID { return i.ID })),
		graphql.Args[pageArgs](),
		graphql.Query(graphql.ResolveArgs("items",
			func(_ context.Context, _ graphql.Root, a pageArgs) ([]*item, error) {
				n := 1
				if a.First != nil {
					n = *a.First
				}
				out := make([]*item, 0, n)
				for i := range n {
					out = append(out, &item{ID: graphql.ID(string(rune('a' + i)))})
				}
				return out, nil
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s, opts...)
}

func shopCtx(shop string) context.Context {
	return context.WithValue(context.Background(), shopKey{}, shop)
}

func keyFunc(ctx context.Context) string {
	s, _ := ctx.Value(shopKey{}).(string)
	return s
}

func status(t *testing.T, resp *graphql.Response) map[string]any {
	t.Helper()
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if cost == nil {
		t.Fatalf("no extensions.cost: %v", resp.Extensions)
	}
	st, _ := cost["throttleStatus"].(map[string]any)
	if st == nil {
		t.Fatalf("no throttleStatus: %v", cost)
	}
	return st
}

func fixedClock(at *time.Time) func() time.Time { return func() time.Time { return *at } }

func TestRequestWithinBudgetPassesAndReportsWhatIsLeft(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 100, RestoreRate: 50, Key: keyFunc, Now: fixedClock(&now),
		}))

	resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items(first: 4) { id } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	st := status(t, resp)
	if st["maximumAvailable"] != 100 || st["restoreRate"] != 50.0 {
		t.Fatalf("throttleStatus = %v", st)
	}
	// items(first: 4) { id } costs 1 + 1*4 = 5.
	if st["currentlyAvailable"] != 95 {
		t.Fatalf("currentlyAvailable = %v, want 95", st["currentlyAvailable"])
	}
}

func TestRequestOverTheRemainingBudgetIsThrottled(t *testing.T) {
	now := time.Unix(0, 0)
	var ran int
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		// The limiter is registered first so it wraps this counter: an
		// interceptor outside it would run even for a rejected request.
		throttle.New(throttle.Config{
			MaximumAvailable: 10, RestoreRate: 1, Key: keyFunc, Now: fixedClock(&now),
		}),
		graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(
			func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
				ran++
				return next(ctx, oc)
			})))

	ctx := shopCtx("shop-1")
	if resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 8) { id } }`}); len(resp.Errors) > 0 {
		t.Fatalf("first request should fit: %v", resp.Errors)
	}
	resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 8) { id } }`})
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "THROTTLED" {
		t.Fatalf("second request should be throttled, got %v", resp.Errors)
	}
	if ran != 1 {
		t.Fatalf("the throttled operation still executed (%d runs)", ran)
	}
	if st := status(t, resp); st["currentlyAvailable"] != 1 {
		t.Fatalf("currentlyAvailable = %v, want 1 (unchanged by the rejection)", st["currentlyAvailable"])
	}
}

func TestPointsRestoreOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 10, RestoreRate: 2, Key: keyFunc, Now: fixedClock(&now),
		}))

	ctx := shopCtx("shop-1")
	e.Execute(ctx, &graphql.Request{Query: `{ items(first: 8) { id } }`}) // 9 points, 1 left
	now = now.Add(3 * time.Second)                                        // +6, capped by maximum
	resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 4) { id } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("restored points should cover this: %v", resp.Errors)
	}
	// 1 + 6 restored = 7, minus 5 = 2.
	if st := status(t, resp); st["currentlyAvailable"] != 2 {
		t.Fatalf("currentlyAvailable = %v, want 2", st["currentlyAvailable"])
	}
}

func TestRestoreIsCappedAtTheMaximum(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 10, RestoreRate: 100, Key: keyFunc, Now: fixedClock(&now),
		}))
	ctx := shopCtx("shop-1")
	e.Execute(ctx, &graphql.Request{Query: `{ items(first: 8) { id } }`})
	now = now.Add(time.Hour)
	resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 1) { id } }`})
	if st := status(t, resp); st["currentlyAvailable"] != 8 {
		t.Fatalf("currentlyAvailable = %v, want 8 (10 restored, minus 2)", st["currentlyAvailable"])
	}
}

func TestBucketsAreIndependentPerKey(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 10, RestoreRate: 1, Key: keyFunc, Now: fixedClock(&now),
		}))
	e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items(first: 8) { id } }`})
	resp := e.Execute(shopCtx("shop-2"), &graphql.Request{Query: `{ items(first: 8) { id } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("shop-2 paid for shop-1: %v", resp.Errors)
	}
}

// A query that could never fit is rejected on its own terms rather than
// waiting for a refill that will never be enough.
func TestQueryLargerThanTheBucketIsRejected(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 10, RestoreRate: 1, Key: keyFunc, Now: fixedClock(&now),
		}))
	resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items(first: 50) { id } }`})
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "THROTTLED" {
		t.Fatalf("errors = %v", resp.Errors)
	}
	if !strings.Contains(resp.Errors[0].Message, "never") {
		t.Fatalf("message should say the query can never fit: %q", resp.Errors[0].Message)
	}
}

// Shopify charges the actual cost, refunding the difference between what the
// query was quoted and what it really resolved.
func TestUnusedPointsAreRefunded(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10, Actual: true}),
		throttle.New(throttle.Config{
			MaximumAvailable: 100, RestoreRate: 1, Key: keyFunc, Now: fixedClock(&now),
		}))
	// No first argument, so the cost model assumes DefaultListSize 10 and
	// quotes 11, while the resolver returns one item and really costs 2.
	resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items { id } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if st := status(t, resp); st["currentlyAvailable"] != 98 {
		t.Fatalf("currentlyAvailable = %v, want 98 (charged the actual 2, not the quoted 11)", st["currentlyAvailable"])
	}
}

// One bucket under concurrent requests must account for every one of them.
// A limiter that loses charges under load is not a limiter.
func TestConcurrentRequestsShareOneBucketExactly(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 1000, RestoreRate: 0, Key: keyFunc, Now: fixedClock(&now),
		}))

	const n = 100
	ctx := shopCtx("shop-1")
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			// items(first: 4) { id } costs 5.
			if resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 4) { id } }`}); len(resp.Errors) > 0 {
				t.Errorf("errors: %v", resp.Errors)
			}
		}()
	}
	wg.Wait()

	resp := e.Execute(ctx, &graphql.Request{Query: `{ items(first: 4) { id } }`})
	if st := status(t, resp); st["currentlyAvailable"] != 1000-(n+1)*5 {
		t.Fatalf("currentlyAvailable = %v, want %d", st["currentlyAvailable"], 1000-(n+1)*5)
	}
}

// A subscription runs the whole operation chain once per event, so each
// event is charged. That is deliberate: a stream billed once at open is an
// unmetered firehose, which is the hole a cost limiter exists to close.
func TestSubscriptionEventsAreChargedIndividually(t *testing.T) {
	ticks := make(chan int, 4)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { hello: String! }
		type Subscription { ticks: Int! }
	`),
		graphql.Query(graphql.Resolve("hello", func(context.Context, graphql.Root) (string, error) {
			return "hi", nil
		})),
		graphql.Subscription(graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) {
			return ticks, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(0, 0)
	e := graphql.NewExecutor(s,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}),
		throttle.New(throttle.Config{
			MaximumAvailable: 100, RestoreRate: 0, Key: keyFunc, Now: fixedClock(&now),
		}))

	ctx, cancel := context.WithCancel(shopCtx("shop-1"))
	defer cancel()
	out, err := e.Subscribe(ctx, &graphql.Request{Query: `subscription { ticks }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// subscription { ticks } costs one point, so each event takes one.
	for i, want := range []int{99, 98, 97} {
		ticks <- i
		resp := <-out
		if got := status(t, resp)["currentlyAvailable"]; got != want {
			t.Fatalf("event %d: currentlyAvailable = %v, want %d", i, got, want)
		}
	}
}
