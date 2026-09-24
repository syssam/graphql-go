package loader_test

import (
	"context"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// unlockAndFlushIfReady releases the coordinator's mutex before running the
// ready callbacks, and says so: "releases w.mu, running the callbacks outside
// it when the operation is ready to dispatch". Running them under the lock
// passes every other test in this repository.
//
// It is not a tidiness point. A flush runs the batch function, and a batch
// function that uses a second loader -- resolve the ids, then load what they
// point at -- calls Load, which calls Park, which takes that same mutex on the
// same goroutine. sync.Mutex is not reentrant, so the request deadlocks until
// its deadline with no error anywhere.
//
// Two loaders where one's batch uses the other is an ordinary shape, not a
// corner: it is what "batch the owners, then batch their organisations" looks
// like.
func TestABatchFunctionMayUseAnotherLoader(t *testing.T) {
	orgs := loader.New(func(_ context.Context, keys []string) (map[string]string, error) {
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			out[k] = "org-" + k
		}
		return out, nil
	})

	// owners' batch function loads through orgs, so the flush that runs it
	// re-enters the coordinator.
	owners := loader.New(func(ctx context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			org, err := orgs.Load(ctx, string(k))
			if err != nil {
				return nil, err
			}
			out[k] = &loadOwner{Name: string(k) + "@" + org}
		}
		return out, nil
	})

	s, err := graphql.NewSchema(graphql.SDL(`
		type User { name: String! }
		type Item { owner: User! }
		type Query { items: [Item!]! }
	`),
		graphql.Object[loadOwner]("User",
			graphql.Field("name", func(u *loadOwner) string { return u.Name }),
		),
		graphql.Object[loadItem]("Item",
			graphql.Resolve("owner", func(ctx context.Context, it *loadItem) (*loadOwner, error) {
				return owners.Load(ctx, it.OwnerID)
			}),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) ([]*loadItem, error) {
			return []*loadItem{{OwnerID: "a"}, {OwnerID: "b"}, {OwnerID: "a"}}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	// The go test -timeout is what catches the deadlock; the assertions below
	// only run if it did not happen.
	resp := run(t, graphql.NewExecutor(s), `{ items { owner { name } } }`, "")
	expectData(t, resp,
		`{"items":[{"owner":{"name":"a@org-a"}},{"owner":{"name":"b@org-b"}},{"owner":{"name":"a@org-a"}}]}`)
}

// The same shape without an executor, so the standalone path -- where the
// scope has no coordinator and schedules its own flush -- is covered too.
func TestAStandaloneBatchFunctionMayUseAnotherLoader(t *testing.T) {
	inner := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		out := make(map[int]int, len(keys))
		for _, k := range keys {
			out[k] = k * 2
		}
		return out, nil
	})
	outer := loader.New(func(ctx context.Context, keys []int) (map[int]int, error) {
		out := make(map[int]int, len(keys))
		for _, k := range keys {
			v, err := inner.Load(ctx, k)
			if err != nil {
				return nil, err
			}
			out[k] = v + 1
		}
		return out, nil
	})
	v, err := outer.Load(context.Background(), 5)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v != 11 {
		t.Errorf("Load = %d, want 11", v)
	}
}
