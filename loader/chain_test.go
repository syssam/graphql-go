package loader_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// chainNode is one link of a chain in which every level is reached through a
// loader of its own: nodes -> next -> next -> next.
type chainNode struct {
	ID    string
	Level int
}

// chain is a schema whose Link.next at level i loads through loaders[i], with
// the number of times each loader's batch function ran.
type chain struct {
	loaders []*loader.Loader[string, *chainNode]
	batches []atomic.Int32
	keys    []atomic.Int32
	// next, when set, replaces the resolver of Link.next; flaky is the
	// resolver of Link.flaky, which otherwise answers null.
	next  func(ctx context.Context, c *chain, n *chainNode) (*chainNode, error)
	flaky func(ctx context.Context, n *chainNode) (*string, error)
}

func newChain(levels int) *chain {
	c := &chain{batches: make([]atomic.Int32, levels), keys: make([]atomic.Int32, levels)}
	for i := range levels {
		c.loaders = append(c.loaders, loader.New(func(_ context.Context, keys []string) (map[string]*chainNode, error) {
			c.batches[i].Add(1)
			c.keys[i].Add(int32(len(keys)))
			out := make(map[string]*chainNode, len(keys))
			for _, k := range keys {
				out[k] = &chainNode{ID: k, Level: i + 1}
			}
			return out, nil
		}))
	}
	return c
}

func (c *chain) reset() {
	for i := range c.batches {
		c.batches[i].Store(0)
		c.keys[i].Store(0)
	}
}

func (c *chain) load(ctx context.Context, n *chainNode) (*chainNode, error) {
	if n.Level >= len(c.loaders) {
		return nil, nil
	}
	return c.loaders[n.Level].Load(ctx, n.ID)
}

func (c *chain) executor(t *testing.T, n int) *graphql.Executor {
	t.Helper()
	nodes := make([]*chainNode, n)
	for i := range nodes {
		nodes[i] = &chainNode{ID: strconv.Itoa(i)}
	}
	s, err := graphql.NewSchema(graphql.SDL(`type Link { id: ID! next: Link flaky: String } type Query { nodes: [Link!]! }`),
		graphql.Object[chainNode]("Link",
			graphql.Field("id", func(n *chainNode) graphql.ID { return graphql.ID(n.ID) }),
			graphql.Resolve("next", func(ctx context.Context, n *chainNode) (*chainNode, error) {
				if c.next != nil {
					return c.next(ctx, c, n)
				}
				return c.load(ctx, n)
			}),
			graphql.Resolve("flaky", func(ctx context.Context, n *chainNode) (*string, error) {
				if c.flaky == nil {
					return nil, nil
				}
				return c.flaky(ctx, n)
			}),
		),
		graphql.Query(graphql.Resolve("nodes", func(context.Context, graphql.Root) ([]*chainNode, error) { return nodes, nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

// chainQuery selects next to the given depth.
func chainQuery(levels int, extra string) string {
	return `{ nodes { ` + extra + strings.Repeat(`next { `, levels) + `id` + strings.Repeat(` }`, levels) + ` } }`
}

// execute runs one request and fails rather than hangs: a wave that is never
// seen as ready leaves a Load parked for as long as its context lives.
func execute(t *testing.T, e *graphql.Executor, query string) *graphql.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *graphql.Response, 1)
	go func() { done <- e.Execute(ctx, &graphql.Request{Query: query}) }()
	select {
	case resp := <-done:
		return resp
	case <-time.After(10 * time.Second):
		t.Fatal("request hung: a Load was left parked in a wave nothing would dispatch")
		return nil
	}
}

// The first level was always one batch, and it was the only level any test
// asked about. Results were delivered to waiters that still counted as parked
// until each was scheduled and reported otherwise itself, so the first one to
// reach its next Load found nothing running and dispatched alone: 100 nodes
// loaded their second level in up to 100 batches.
//
// Real goroutines on purpose. The failure is a scheduling race, which
// testing/synctest serializes away.
func TestEveryLevelOfALoaderChainIsOneBatch(t *testing.T) {
	const n = 100
	for _, levels := range []int{2, 3} {
		t.Run(strconv.Itoa(levels)+" levels", func(t *testing.T) {
			c := newChain(levels)
			e := c.executor(t, n)
			query := chainQuery(levels, "")
			for run := range 300 {
				c.reset()
				resp := execute(t, e, query)
				if len(resp.Errors) > 0 {
					t.Fatalf("errors: %v", resp.Errors)
				}
				for level := range levels {
					if got := c.batches[level].Load(); got != 1 || c.keys[level].Load() != n {
						t.Fatalf("request %d: level %d loaded %d keys in %d batches, want %d keys in 1", run, level+1, c.keys[level].Load(), got, n)
					}
				}
				resp.Release()
			}
		})
	}
}

// A waiter that stops waiting has reported itself running again, and the batch
// that finishes afterwards must not report it a second time: counted twice, it
// is running forever and the next level is never dispatched.
func TestWaitersThatGiveUpLeaveTheNextLevelOneBatch(t *testing.T) {
	const n = 20
	slow := loader.New(func(_ context.Context, keys []string) (map[string]string, error) {
		time.Sleep(50 * time.Millisecond)
		return map[string]string{}, nil
	})
	c := newChain(2)
	c.flaky = func(ctx context.Context, n *chainNode) (*string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
		defer cancel()
		v, err := slow.Load(ctx, n.ID)
		return &v, err
	}
	e := c.executor(t, n)
	for range 20 {
		c.reset()
		resp := execute(t, e, chainQuery(2, "flaky "))
		gaveUp := 0
		for _, err := range resp.Errors {
			if errors.Is(err, context.DeadlineExceeded) {
				gaveUp++
			}
		}
		if gaveUp == 0 {
			t.Fatalf("no waiter gave up, so the case was not exercised: %v", resp.Errors)
		}
		for level := range 2 {
			if got := c.batches[level].Load(); got != 1 || c.keys[level].Load() != n {
				t.Fatalf("level %d loaded %d keys in %d batches, want %d keys in 1", level+1, c.keys[level].Load(), got, n)
			}
		}
		resp.Release()
	}
}

// A waiter whose resolver panics once its Load has returned, and one that
// returns without loading again, both leave the wave through the end of their
// task. Neither may be left counted as parked or as running.
func TestWaitersThatPanicOrStopLeaveTheNextLevelOneBatch(t *testing.T) {
	const n = 60
	c := newChain(3)
	c.next = func(ctx context.Context, c *chain, n *chainNode) (*chainNode, error) {
		next, err := c.load(ctx, n)
		id, _ := strconv.Atoi(n.ID)
		switch {
		case n.Level == 0 && id%3 == 1:
			panic("after the load")
		case n.Level == 1 && id%3 == 2:
			return nil, nil
		}
		return next, err
	}
	e := c.executor(t, n)
	for range 50 {
		c.reset()
		resp := execute(t, e, chainQuery(3, ""))
		if len(resp.Errors) != n/3 {
			t.Fatalf("errors = %d, want one per panicking resolver (%d)", len(resp.Errors), n/3)
		}
		// Level 1 loads every node; a third panic afterwards. Level 2 is asked
		// for by the two thirds that are left, and a third of the nodes then
		// stop, so level 3 is asked for by one third.
		for level, want := range []int32{n, 2 * n / 3, n / 3} {
			if got := c.batches[level].Load(); got != 1 || c.keys[level].Load() != want {
				t.Fatalf("level %d loaded %d keys in %d batches, want %d keys in 1", level+1, c.keys[level].Load(), got, want)
			}
		}
		resp.Release()
	}
}

// A resolver may load from goroutines of its own, which the executor does not
// count. One of them can be dispatched for before it has parked; it must then
// neither park nor be reported running, or the count is off for the rest of
// the request and the Load that follows waits for a dispatch that cannot come.
func TestLoadsFromAResolversOwnGoroutinesLeaveTheWaveBalanced(t *testing.T) {
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		out := make(map[int]int, len(keys))
		for _, k := range keys {
			out[k] = k * 2
		}
		return out, nil
	})
	fan := func(ctx context.Context, _ graphql.Root) (int, error) {
		var wg sync.WaitGroup
		var sum atomic.Int64
		for i := range 8 {
			wg.Go(func() {
				if v, err := ld.Load(ctx, i); err == nil {
					sum.Add(int64(v))
				}
			})
		}
		wg.Wait()
		v, err := ld.Load(ctx, 100)
		return int(sum.Load()) + v, err
	}
	s, err := graphql.NewSchema(graphql.SDL(`type Query { a: Int! b: Int! }`),
		graphql.Query(graphql.Resolve("a", fan), graphql.Resolve("b", fan)),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	for range 300 {
		resp := execute(t, e, `{ a b }`)
		if got, want := string(resp.Data), `{"a":256,"b":256}`; got != want || len(resp.Errors) > 0 {
			t.Fatalf("data = %s, errors = %v; want %s", got, resp.Errors, want)
		}
		resp.Release()
	}
}
