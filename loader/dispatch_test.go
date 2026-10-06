package loader_test

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

type dispatchItem struct{ ID string }

func dispatchItems(n int) []*dispatchItem {
	items := make([]*dispatchItem, n)
	for i := range items {
		items[i] = &dispatchItem{ID: strconv.Itoa(i)}
	}
	return items
}

func echoBatch(calls *atomic.Int32) func(context.Context, []string) (map[string]string, error) {
	return func(_ context.Context, keys []string) (map[string]string, error) {
		calls.Add(1)
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			out[k] = k
		}
		return out, nil
	}
}

// Two loaders queued in one wave are two independent round trips. They ran one
// after the other on the goroutine that found the wave ready, so a type with
// five loader-backed fields waited for the sum of five queries. Each batch
// function here waits for the other to have started, which only happens when
// both are running at once.
func TestLoadersQueuedInOneWaveRunTogether(t *testing.T) {
	startedA, startedB := make(chan struct{}), make(chan struct{})
	var alone atomic.Int32
	meet := func(mine, other chan struct{}) func(context.Context, []string) (map[string]string, error) {
		return func(_ context.Context, keys []string) (map[string]string, error) {
			close(mine)
			select {
			case <-other:
			case <-time.After(2 * time.Second):
				alone.Add(1)
			}
			out := make(map[string]string, len(keys))
			for _, k := range keys {
				out[k] = k
			}
			return out, nil
		}
	}
	a, b := loader.New(meet(startedA, startedB)), loader.New(meet(startedB, startedA))
	s, err := graphql.NewSchema(graphql.SDL(`type Item { a: String! b: String! } type Query { items: [Item!]! }`),
		graphql.Object[dispatchItem]("Item",
			graphql.Resolve("a", func(ctx context.Context, it *dispatchItem) (string, error) { return a.Load(ctx, it.ID) }),
			graphql.Resolve("b", func(ctx context.Context, it *dispatchItem) (string, error) { return b.Load(ctx, it.ID) }),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) ([]*dispatchItem, error) { return dispatchItems(3), nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := execute(t, graphql.NewExecutor(s), `{ items { a b } }`)
	expectData(t, resp, `{"items":[{"a":"0","b":"0"},{"a":"1","b":"1"},{"a":"2","b":"2"}]}`)
	if n := alone.Load(); n != 0 {
		t.Fatalf("%d of the two batch functions never saw the other running: they ran one after the other", n)
	}
}

// What two loaders in one wave lead to is one wave as well, so no result is
// handed back until every batch in the wave has returned. Delivering each
// loader's as its own batch function returned let the first loader's
// resolvers run ahead and be dispatched for before the second loader's had
// been woken, and the level they share was loaded in two batches. manager is
// the slower of the two here, so anything that does not wait for it shows.
func TestTwoLoadersFeedOneBatchAtTheNextLevel(t *testing.T) {
	const n = 40
	var companies, managers, owners atomic.Int32
	company, owner := loader.New(echoBatch(&companies)), loader.New(echoBatch(&owners))
	manager := loader.New(func(ctx context.Context, keys []string) (map[string]string, error) {
		time.Sleep(5 * time.Millisecond)
		return echoBatch(&managers)(ctx, keys)
	})
	type party struct{ ID string }
	s, err := graphql.NewSchema(graphql.SDL(`
		type Party { owner: String! }
		type Item { company: Party! manager: Party! }
		type Query { items: [Item!]! }`),
		graphql.Object[party]("Party",
			graphql.Resolve("owner", func(ctx context.Context, p *party) (string, error) { return owner.Load(ctx, p.ID) }),
		),
		graphql.Object[dispatchItem]("Item",
			graphql.Resolve("company", func(ctx context.Context, it *dispatchItem) (*party, error) {
				id, err := company.Load(ctx, it.ID)
				return &party{ID: "c" + id}, err
			}),
			graphql.Resolve("manager", func(ctx context.Context, it *dispatchItem) (*party, error) {
				id, err := manager.Load(ctx, it.ID)
				return &party{ID: "m" + id}, err
			}),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) ([]*dispatchItem, error) { return dispatchItems(n), nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	for run := range 50 {
		companies.Store(0)
		managers.Store(0)
		owners.Store(0)
		resp := execute(t, e, `{ items { company { owner } manager { owner } } }`)
		if len(resp.Errors) > 0 {
			t.Fatalf("errors: %v", resp.Errors)
		}
		if c, m, o := companies.Load(), managers.Load(), owners.Load(); c != 1 || m != 1 || o != 1 {
			t.Fatalf("request %d: batches company=%d manager=%d owner=%d, want one each", run, c, m, o)
		}
		resp.Release()
	}
}

// A batch is dispatched when nothing in the operation is left running, so it
// waits for a resolver that has nothing to do with it. That is the documented
// cost of never dispatching while a key can still be queued, and this pins it:
// a change that dispatches early is a change to the batching guarantee.
func TestABatchWaitsForEveryRunningResolver(t *testing.T) {
	var batches atomic.Int32
	ld := loader.New(echoBatch(&batches))
	release := make(chan struct{})
	s, err := graphql.NewSchema(graphql.SDL(`type Item { owner: String! } type Query { slow: Int! items: [Item!]! }`),
		graphql.Object[dispatchItem]("Item",
			graphql.Resolve("owner", func(ctx context.Context, it *dispatchItem) (string, error) { return ld.Load(ctx, it.ID) }),
		),
		graphql.Query(
			graphql.Resolve("slow", func(context.Context, graphql.Root) (int, error) {
				<-release
				return 1, nil
			}),
			graphql.Resolve("items", func(context.Context, graphql.Root) ([]*dispatchItem, error) { return dispatchItems(3), nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *graphql.Response, 1)
	go func() {
		done <- graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: `{ slow items { owner } }`})
	}()
	select {
	case <-done:
		t.Fatal("the request finished while slow was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if n := batches.Load(); n != 0 {
		t.Fatalf("the batch ran %d times while a resolver was still running", n)
	}
	close(release)
	select {
	case resp := <-done:
		expectData(t, resp, `{"slow":1,"items":[{"owner":"0"},{"owner":"1"},{"owner":"2"}]}`)
	case <-time.After(5 * time.Second):
		t.Fatal("request hung after slow returned")
	}
	if n := batches.Load(); n != 1 {
		t.Fatalf("batches = %d, want 1", n)
	}
}

// The goroutine that launched a group of fields counts as running again once
// it stops waiting for them, and it must: it goes on to launch the next group.
// Left uncounted, the second group here is one short, so when early parks the
// operation reads as idle while late has not loaded yet, and each gets a batch
// of its own.
func TestALaunchersSecondGroupIsOneBatch(t *testing.T) {
	var batches atomic.Int32
	ld := loader.New(echoBatch(&batches))
	type box struct{}
	type plain struct{}
	type loads struct{}
	s, err := graphql.NewSchema(graphql.SDL(`
		type Plain { x: String! y: String! }
		type Loads { early: String! late: String! }
		type Box { first: Plain! second: Loads! }
		type Query { box: Box! }`),
		graphql.Object[plain]("Plain",
			graphql.Resolve("x", func(context.Context, *plain) (string, error) { return "x", nil }),
			graphql.Resolve("y", func(context.Context, *plain) (string, error) { return "y", nil }),
		),
		graphql.Object[loads]("Loads",
			graphql.Resolve("early", func(ctx context.Context, _ *loads) (string, error) { return ld.Load(ctx, "early") }),
			graphql.Resolve("late", func(ctx context.Context, _ *loads) (string, error) {
				time.Sleep(50 * time.Millisecond)
				return ld.Load(ctx, "late")
			}),
		),
		// Pure fields, so one goroutine writes first's group and then second's.
		graphql.Object[box]("Box",
			graphql.Field("first", func(*box) *plain { return &plain{} }),
			graphql.Field("second", func(*box) *loads { return &loads{} }),
		),
		graphql.Query(graphql.Field("box", func(graphql.Root) *box { return &box{} })),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := execute(t, graphql.NewExecutor(s), `{ box { first { x y } second { early late } } }`)
	expectData(t, resp, `{"box":{"first":{"x":"x","y":"y"},"second":{"early":"early","late":"late"}}}`)
	if n := batches.Load(); n != 1 {
		t.Fatalf("batches = %d, want 1: early was dispatched while late had still to load", n)
	}
}
