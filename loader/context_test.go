package loader_test

import (
	"context"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

type ctxUser struct{ ID int }

// A resolver that narrows its own context and returns cancels that context.
// The batch for a later wave must not inherit it: the scope used to keep the
// first Load's context for the whole request, so `friend` below failed with
// context.Canceled and nulled the response.
func TestALaterWaveDoesNotInheritAnEarlierLoadsContext(t *testing.T) {
	ld := loader.New(func(ctx context.Context, keys []int) (map[int]*ctxUser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out := make(map[int]*ctxUser, len(keys))
		for _, k := range keys {
			out[k] = &ctxUser{ID: k}
		}
		return out, nil
	})
	s, err := graphql.NewSchema(graphql.SDL(`
		type User { id: Int! friend: User }
		type Query { me: User }`),
		graphql.Object[ctxUser]("User",
			graphql.Field("id", func(u *ctxUser) int { return u.ID }),
			graphql.Resolve("friend", func(ctx context.Context, u *ctxUser) (*ctxUser, error) {
				return ld.Load(ctx, u.ID+1)
			}),
		),
		graphql.Query(
			graphql.Resolve("me", func(ctx context.Context, _ graphql.Root) (*ctxUser, error) {
				ctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return ld.Load(ctx, 1)
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := run(t, graphql.NewExecutor(s), `{ me { id friend { id } } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got, want := string(resp.Data), `{"me":{"id":1,"friend":{"id":2}}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// Outside Execute and outside WithScope every caller in the process shares
// one scope, so it must not cache; WithScope gives each context its own.
func TestOnlyAScopedLoadIsCachedOutsideExecute(t *testing.T) {
	var calls int
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		calls++
		return map[int]int{1: 10}, nil
	})
	for range 2 {
		if v, err := ld.Load(context.Background(), 1); err != nil || v != 10 {
			t.Fatalf("Load = (%d, %v)", v, err)
		}
	}
	if calls != 2 {
		t.Fatalf("unscoped: batch ran %d times for two Loads, want 2 (nothing cached)", calls)
	}
	calls = 0
	a, b := loader.WithScope(context.Background()), loader.WithScope(context.Background())
	for _, ctx := range []context.Context{a, a, b} {
		if v, err := ld.Load(ctx, 1); err != nil || v != 10 {
			t.Fatalf("Load = (%d, %v)", v, err)
		}
	}
	if calls != 2 {
		t.Errorf("scoped: batch ran %d times, want 2 (one per scope)", calls)
	}
}
