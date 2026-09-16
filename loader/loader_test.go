package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

type tenantKey struct{}

// The batch function's ctx parameter must be the request's context: a batch
// that cannot see cancellation keeps querying for a client that has gone.
func TestLoaderBatchObservesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan error, 1)
	ld := loader.New(func(ctx context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		close(started)
		select {
		case <-ctx.Done():
			observed <- ctx.Err()
		case <-time.After(2 * time.Second):
			observed <- nil
		}
		return nil, ctx.Err()
	})

	e := newLoaderSchema(t, ld, []*loadItem{{"1"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	e.Execute(ctx, &graphql.Request{Query: `{ items { owner { name } } }`})

	if err := <-observed; err == nil {
		t.Fatal("batch function never saw the request cancellation")
	}
}

// Request-scoped values (tenant, auth, trace span) must reach the batch
// function; it is the only place a DataLoader-backed query can read them.
func TestLoaderBatchSeesRequestValues(t *testing.T) {
	var got atomic.Value
	ld := loader.New(func(ctx context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		if v, ok := ctx.Value(tenantKey{}).(string); ok {
			got.Store(v)
		}
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			out[k] = &loadOwner{Name: "u" + string(k)}
		}
		return out, nil
	})

	e := newLoaderSchema(t, ld, []*loadItem{{"1"}})
	ctx := context.WithValue(context.Background(), tenantKey{}, "tenant-42")
	resp := e.Execute(ctx, &graphql.Request{Query: `{ items { owner { name } } }`})
	expectData(t, resp, `{"items":[{"owner":{"name":"u1"}}]}`)

	if got.Load() != "tenant-42" {
		t.Fatalf("batch function saw tenant %v, want tenant-42", got.Load())
	}
}

// A deadline on the request must bound the batch call too.
func TestLoaderBatchSeesRequestDeadline(t *testing.T) {
	var hasDeadline atomic.Bool
	ld := loader.New(func(ctx context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		_, ok := ctx.Deadline()
		hasDeadline.Store(ok)
		return map[graphql.ID]*loadOwner{"1": {Name: "u1"}}, nil
	})

	e := newLoaderSchema(t, ld, []*loadItem{{"1"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	expectData(t, e.Execute(ctx, &graphql.Request{Query: `{ items { owner { name } } }`}),
		`{"items":[{"owner":{"name":"u1"}}]}`)

	if !hasDeadline.Load() {
		t.Fatal("batch function saw no deadline; the request's was not propagated")
	}
}

// newNullableOwnerSchema is newLoaderSchema with a nullable owner, so one
// failed key is visible as a null beside its surviving siblings instead of
// bubbling the whole list away.
func newNullableOwnerSchema(t *testing.T, ld *loader.Loader[graphql.ID, *loadOwner], items []*loadItem) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`
		type User { name: String! }
		type Item { owner: User }
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

// GraphQL's partial-response contract is per field, so one key failing in a
// batch must not null its siblings.
func TestMappedLoaderReportsPerKeyErrors(t *testing.T) {
	ld := loader.NewMapped(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, map[graphql.ID]error, error) {
		vals := make(map[graphql.ID]*loadOwner, len(keys))
		errs := make(map[graphql.ID]error)
		for _, k := range keys {
			if k == "bad" {
				errs[k] = errors.New("owner lookup failed")
				continue
			}
			vals[k] = &loadOwner{Name: "u" + string(k)}
		}
		return vals, errs, nil
	})

	e := newNullableOwnerSchema(t, ld, []*loadItem{{"1"}, {"bad"}, {"2"}})
	resp := e.Execute(context.Background(), &graphql.Request{Query: `{ items { owner { name } } }`})

	want := `{"items":[{"owner":{"name":"u1"}},{"owner":null},{"owner":{"name":"u2"}}]}`
	if got := string(resp.Data); got != want {
		t.Fatalf("one failed key took its siblings with it\n got: %s\nwant: %s", got, want)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %d, want exactly 1", len(resp.Errors))
	}
	if !strings.Contains(resp.Errors[0].Message, "owner lookup failed") {
		t.Fatalf("error message = %q", resp.Errors[0].Message)
	}
}

// A batch-wide failure (the database is down) still fails every key in it.
func TestMappedLoaderBatchErrorFailsEveryKey(t *testing.T) {
	ld := loader.NewMapped(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, map[graphql.ID]error, error) {
		return nil, nil, errors.New("database unavailable")
	})

	e := newNullableOwnerSchema(t, ld, []*loadItem{{"1"}, {"2"}})
	resp := e.Execute(context.Background(), &graphql.Request{Query: `{ items { owner { name } } }`})

	if got := string(resp.Data); got != `{"items":[{"owner":null},{"owner":null}]}` {
		t.Fatalf("data = %s", got)
	}
	if len(resp.Errors) != 2 {
		t.Fatalf("errors = %d, want 2 (one per key)", len(resp.Errors))
	}
}
