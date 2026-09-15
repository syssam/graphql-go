package loader_test

import (
	"context"
	"encoding/json"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

type loadOwner struct{ Name string }
type loadItem struct{ OwnerID graphql.ID }

func newLoaderSchema(t *testing.T, ld *loader.Loader[graphql.ID, *loadOwner], items []*loadItem) *graphql.Executor {
	t.Helper()
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
				return ld.Load(ctx, it.OwnerID)
			}),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) ([]*loadItem, error) {
			return items, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

func TestLoaderBatchesConcurrentResolvers(t *testing.T) {
	var mu sync.Mutex
	var batches [][]graphql.ID
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		mu.Lock()
		batches = append(batches, append([]graphql.ID(nil), keys...))
		mu.Unlock()
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			out[k] = &loadOwner{Name: "u" + string(k)}
		}
		return out, nil
	})

	e := newLoaderSchema(t, ld, []*loadItem{{"1"}, {"2"}, {"1"}})
	resp := run(t, e, `{ items { owner { name } } }`, "")
	expectData(t, resp, `{"items":[{"owner":{"name":"u1"}},{"owner":{"name":"u2"}},{"owner":{"name":"u1"}}]}`)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 (N+1 not coalesced): %v", len(batches), batches)
	}
	if len(batches[0]) != 2 {
		t.Fatalf("batch keys = %v, want 2 unique ids", batches[0])
	}
}

func newLoaderSeqSchema(t *testing.T, ld *loader.Loader[graphql.ID, *loadOwner], items []*loadItem) *graphql.Executor {
	t.Helper()
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
				return ld.Load(ctx, it.OwnerID)
			}),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) (iter.Seq[*loadItem], error) {
			return func(yield func(*loadItem) bool) {
				for _, it := range items {
					if !yield(it) {
						return
					}
				}
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

// A seq list whose element has a resolver must still batch. The concurrent
// path drains the seq before announcing the wave, and that drain is what
// keeps batching intact. Two batches here means something began streaming
// into pushWave, which is the N+1 regression this engine exists to avoid.
func TestLoaderBatchesSeqList(t *testing.T) {
	var mu sync.Mutex
	var batches [][]graphql.ID
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		mu.Lock()
		batches = append(batches, append([]graphql.ID(nil), keys...))
		mu.Unlock()
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			out[k] = &loadOwner{Name: "u" + string(k)}
		}
		return out, nil
	})

	e := newLoaderSeqSchema(t, ld, []*loadItem{{"1"}, {"2"}, {"1"}})
	resp := run(t, e, `{ items { owner { name } } }`, "")
	expectData(t, resp, `{"items":[{"owner":{"name":"u1"}},{"owner":{"name":"u2"}},{"owner":{"name":"u1"}}]}`)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 (N+1 not coalesced): %v", len(batches), batches)
	}
	if len(batches[0]) != 2 {
		t.Fatalf("batch keys = %v, want 2 unique ids", batches[0])
	}
}

func TestLoaderCacheIsPerRequest(t *testing.T) {
	var n atomic.Int32
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		n.Add(1)
		return map[graphql.ID]*loadOwner{keys[0]: {Name: "a"}}, nil
	})
	e := newLoaderSchema(t, ld, []*loadItem{{"1"}})
	expectData(t, run(t, e, `{ items { owner { name } } }`, ""), `{"items":[{"owner":{"name":"a"}}]}`)
	expectData(t, run(t, e, `{ items { owner { name } } }`, ""), `{"items":[{"owner":{"name":"a"}}]}`)
	if got := n.Load(); got != 2 {
		t.Fatalf("batch calls = %d, want 2 (one per request)", got)
	}
}

func TestLoaderPrimeAndClear(t *testing.T) {
	var n atomic.Int32
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]string, error) {
		n.Add(1)
		return map[graphql.ID]string{"1": "from-batch"}, nil
	})
	s, err := graphql.NewSchema(graphql.SDL(`type Query { a: String! b: String! }`),
		graphql.Query(
			graphql.Resolve("a", func(ctx context.Context, _ graphql.Root) (string, error) {
				ld.Prime(ctx, "1", "primed")
				return ld.Load(ctx, "1")
			}),
			graphql.Resolve("b", func(ctx context.Context, _ graphql.Root) (string, error) {
				ld.Clear(ctx, "1")
				return ld.Load(ctx, "1")
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	expectData(t, run(t, e, `{ a }`, ""), `{"a":"primed"}`)
	if n.Load() != 0 {
		t.Fatal("Prime must skip the batch function")
	}
	expectData(t, run(t, e, `{ b }`, ""), `{"b":"from-batch"}`)
	if n.Load() != 1 {
		t.Fatalf("Clear then Load should batch once, got %d", n.Load())
	}
}

func TestLoaderLoadMany(t *testing.T) {
	var got []graphql.ID
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]string, error) {
		got = append([]graphql.ID(nil), keys...)
		return map[graphql.ID]string{"a": "A", "b": "B"}, nil
	})
	s, err := graphql.NewSchema(graphql.SDL(`type Query { pair: [String!]! }`),
		graphql.Query(graphql.Resolve("pair", func(ctx context.Context, _ graphql.Root) ([]string, error) {
			return ld.LoadMany(ctx, []graphql.ID{"a", "b"})
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	expectData(t, run(t, graphql.NewExecutor(s), `{ pair }`, ""), `{"pair":["A","B"]}`)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("keys = %v", got)
	}
}

func TestLoaderStandalone(t *testing.T) {
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		return map[int]int{7: 49}, nil
	})
	v, err := ld.Load(context.Background(), 7)
	if err != nil || v != 49 {
		t.Fatalf("standalone Load = (%d, %v)", v, err)
	}
}

func TestLoaderMissingKeyIsZero(t *testing.T) {
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		return map[graphql.ID]*loadOwner{}, nil
	})
	e := newLoaderSchema(t, ld, []*loadItem{{"missing"}})
	resp := run(t, e, `{ items { owner { name } } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("nil owner on non-null field must bubble")
	}
}

func run(t *testing.T, e *graphql.Executor, query, vars string) *graphql.Response {
	t.Helper()
	req := &graphql.Request{Query: query}
	if vars != "" {
		req.Variables = json.RawMessage(vars)
	}
	return e.Execute(context.Background(), req)
}

// expectData asserts the exact data payload and no errors.
func expectData(t *testing.T, resp *graphql.Response, want string) {
	t.Helper()
	if len(resp.Errors) > 0 {
		b, _ := json.Marshal(resp.Errors)
		t.Fatalf("unexpected errors: %s", b)
	}
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestLoaderScopeSharedAcrossConcurrentResolvers pins the request scope:
// resolvers that first touch a Loader concurrently must share one scope, or
// each builds its own pending queue and cache and batching degrades to N+1.
func TestLoaderScopeSharedAcrossConcurrentResolvers(t *testing.T) {
	var mu sync.Mutex
	var batches [][]graphql.ID
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		mu.Lock()
		batches = append(batches, append([]graphql.ID(nil), keys...))
		mu.Unlock()
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			out[k] = &loadOwner{Name: string(k)}
		}
		return out, nil
	})

	const n = 16
	items := make([]*loadItem, n)
	want := make([]string, n)
	for i := range items {
		id := graphql.ID(strconv.Itoa(i))
		items[i] = &loadItem{OwnerID: id}
		want[i] = `{"owner":{"name":"` + string(id) + `"}}`
	}

	e := newLoaderSchema(t, ld, items)
	expectData(t, run(t, e, `{ items { owner { name } } }`, ""),
		`{"items":[`+strings.Join(want, ",")+`]}`)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1; concurrent first-touch must share one scope: %v", len(batches), batches)
	}
}
