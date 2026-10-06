package graphql

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An input type that refers to itself is the shape @authorizeInput exists
// for -- a filter with and/or/not -- and it is the one whose depth the client
// chooses rather than the schema. Literals are held to the document's nesting
// limit; a variable is not, and the variables decoder admits 10 000 levels.
const deepInputSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Deep { not: Deep name: String tag: String }
type Message { id: ID! secret: String! }
type Query { deep(where: Deep @authorizeInput(kind: FILTER)): String! }
type Subscription { deepEvents(where: Deep @authorizeInput(kind: FILTER)): Message! }
`

type deepIn struct{ Not *deepIn }

type deepArgs struct{ Where *deepIn }

// deepInputSeen is what one Authorize call was shown for the argument.
type deepInputSeen struct {
	calls   atomic.Int64
	keys    atomic.Int64
	longest atomic.Int64
}

func newDeepInputExecutor(t *testing.T) (*Executor, *deepInputSeen, *authSubSource, *atomic.Int64) {
	t.Helper()
	src := &authSubSource{ch: make(chan *authSubMessage)}
	resolved := new(atomic.Int64)
	s, err := NewSchema(SDL(deepInputSDL),
		Input[deepIn]("Deep",
			InputField("not", func(d *deepIn, v *deepIn) { d.Not = v }),
			InputField("name", func(*deepIn, *string) {}),
			InputField("tag", func(*deepIn, *string) {}),
		),
		Args[deepArgs](InputField("where", func(a *deepArgs, v *deepIn) { a.Where = v })),
		Object[authSubMessage]("Message",
			Field("id", func(m *authSubMessage) ID { return ID(m.ID) }),
			Field("secret", func(m *authSubMessage) string { return m.Secret }),
		),
		Query(FieldArgs("deep", func(Root, deepArgs) string {
			resolved.Add(1)
			return "ok"
		})),
		Subscription(SubscribeArgs("deepEvents", func(context.Context, deepArgs) (<-chan *authSubMessage, error) {
			src.opens.Add(1)
			return src.ch, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	seen := &deepInputSeen{}
	a := AuthorizerFunc(func(_ context.Context, shape *AuthShape, d *Decision) error {
		seen.calls.Add(1)
		keys, longest := 0, 0
		for i := range shape.Sites() {
			for _, k := range d.Input(i) {
				keys++
				longest = max(longest, len(k.Path))
			}
		}
		seen.keys.Store(int64(keys))
		seen.longest.Store(int64(longest))
		return nil
	})
	return NewExecutor(s, WithAuthorizer(a)), seen, src, resolved
}

// deepChain nests depth objects under "not" with one scalar at the bottom:
// every reported path is a prefix of the next one.
func deepChain(depth int) string {
	return `{"w":` + strings.Repeat(`{"not":`, depth) + `{"name":"x"}` + strings.Repeat(`}`, depth) + `}`
}

// deepComb is the chain with a scalar at every level as well, so each level
// reports a path that ends where no deeper one continues.
func deepComb(depth int) string {
	return `{"w":` + strings.Repeat(`{"name":"x","tag":"y","not":`, depth) + `{"name":"x"}` + strings.Repeat(`}`, depth) + `}`
}

// allocatedBytes is what fn allocated, as the least of three runs so that a
// collection or a stray goroutine in one of them does not decide the result.
func allocatedBytes(fn func()) uint64 {
	least := ^uint64(0)
	for range 3 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		fn()
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least
}

// The ceiling the three tests below hold an authorized request to. It is far
// above what the walk needs -- a megabyte or two at the deepest input the
// decoder admits -- and far below what cloning a path per key cost there,
// which was two gigabytes for a 79 KB request.
const deepInputCeiling = 64 << 20

// The walk runs before the Authorizer is asked anything, so its cost is paid
// for every caller, whoever they turn out to be. It must be linear in the
// input: one request must not be able to make the engine allocate thousands
// of times its own size.
func TestAuthorizedInputWalkIsLinearInNestingDepth(t *testing.T) {
	e, seen, _, resolved := newDeepInputExecutor(t)
	const depth = 9900
	vars := deepChain(depth)

	var resp *Response
	got := allocatedBytes(func() {
		resp = run(t, e, `query($w: Deep) { deep(where: $w) }`, vars)
	})
	// Nothing about the deepest input the decoder admits is refused: the
	// bound is on space, and a chain shares all of it.
	expectData(t, resp, `{"deep":"ok"}`)
	if resolved.Load() == 0 {
		t.Fatal("the field did not resolve")
	}
	if keys, longest := seen.keys.Load(), seen.longest.Load(); keys != depth+1 || longest != depth+1 {
		t.Fatalf("the Authorizer saw %d keys, the longest %d deep; want %d of each", keys, longest, depth+1)
	}
	if got > deepInputCeiling {
		t.Fatalf("a %d-byte request nested %d deep allocated %d MB; the walk is not linear in its input",
			len(vars), depth, got>>20)
	}
}

// A comb cannot be reported in linear space whatever the walk does, because a
// key's Path is a slice of its own: n levels each ending in a scalar are n
// paths of up to n elements. So past a bound the operation is refused, before
// the Authorizer or the resolver sees any of it -- a policy shown part of an
// argument would be deciding on less than the resolver was about to receive.
func TestAuthorizedInputTooLargeToReportIsRefused(t *testing.T) {
	e, seen, _, resolved := newDeepInputExecutor(t)
	const depth = 3000
	vars := deepComb(depth)

	var resp *Response
	got := allocatedBytes(func() {
		resp = run(t, e, `query($w: Deep) { deep(where: $w) }`, vars)
	})
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeBadUserInput ||
		!strings.Contains(resp.Errors[0].Message, "Query.deep(where:)") {
		t.Fatalf("errors = %s, want one BAD_USER_INPUT naming the argument", errorsJSON(resp.Errors))
	}
	if resp.Data != nil {
		t.Fatalf("data = %s, want none: the operation is refused whole", resp.Data)
	}
	if seen.calls.Load() != 0 || resolved.Load() != 0 {
		t.Fatalf("Authorize ran %d times and the resolver %d; neither may run on part of an argument",
			seen.calls.Load(), resolved.Load())
	}
	if got > deepInputCeiling {
		t.Fatalf("refusing a %d-byte request allocated %d MB", len(vars), got>>20)
	}

	// The bound is not a limit on depth or on combs as such: one a real
	// filter could plausibly be is authorized in full.
	resp = run(t, e, `query($w: Deep) { deep(where: $w) }`, deepComb(100))
	expectData(t, resp, `{"deep":"ok"}`)
	if keys := seen.keys.Load(); keys != 2*100+100+1 {
		t.Fatalf("the Authorizer saw %d keys of a comb 100 deep, want %d", keys, 2*100+100+1)
	}
}

// The budget is the request's, not the argument's: a client must not get it
// again for every alias it writes.
func TestAuthorizedInputBudgetIsSharedByEveryArgumentSite(t *testing.T) {
	e, _, _, resolved := newDeepInputExecutor(t)
	// One comb this deep is well inside the budget; sixty-four are not.
	const depth = 100
	one := run(t, e, `query($w: Deep) { deep(where: $w) }`, deepComb(depth))
	expectData(t, one, `{"deep":"ok"}`)
	resolved.Store(0)

	var q strings.Builder
	q.WriteString(`query($w: Deep) {`)
	for i := range 64 {
		q.WriteString(` a`)
		q.WriteString(strings.Repeat("a", i))
		q.WriteString(`: deep(where: $w)`)
	}
	q.WriteString(` }`)
	resp := run(t, e, q.String(), deepComb(depth))
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeBadUserInput {
		t.Fatalf("errors = %s, want one BAD_USER_INPUT: every site walked the same variable again", errorsJSON(resp.Errors))
	}
	if resolved.Load() != 0 {
		t.Fatalf("%d fields resolved in an operation that was refused", resolved.Load())
	}
}

// A subscription re-authorizes every event, and so walks its arguments again
// for every event, for as long as the stream lives. What one request could
// cost once, a stream could cost per event.
func TestSubscriptionEventWalksAuthorizedInputInBoundedSpace(t *testing.T) {
	e, seen, src, _ := newDeepInputExecutor(t)
	const depth, events = 9900, 3
	vars := deepChain(depth)

	ch, err := e.Subscribe(t.Context(), &Request{
		Query:     `subscription($w: Deep) { deepEvents(where: $w) { id } }`,
		Variables: []byte(vars),
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if src.opens.Load() != 1 || seen.calls.Load() != 1 {
		t.Fatalf("opens = %d, Authorize calls = %d, want 1 and 1", src.opens.Load(), seen.calls.Load())
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range events {
		sendEvent(t, src.ch, &authSubMessage{ID: "m"})
		select {
		case resp := <-ch:
			expectData(t, resp, `{"deepEvents":{"id":"m"}}`)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for an event")
		}
	}
	runtime.ReadMemStats(&after)

	if calls := seen.calls.Load(); calls != 1+events {
		t.Fatalf("Authorize ran %d times, want once at open and once per event", calls)
	}
	if keys := seen.keys.Load(); keys != depth+1 {
		t.Fatalf("an event's Authorizer saw %d keys, want %d", keys, depth+1)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > events*deepInputCeiling {
		t.Fatalf("%d events allocated %d MB", events, got>>20)
	}
}

// Sharing prefixes is what makes the chain cheap, and the ceiling above is
// loose enough that losing most of it would still pass. This holds the walk
// itself to a small multiple of its input.
func TestInputKeysShareThePathOfAChain(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	const depth = 4000
	chain := any(map[string]any{"nameContains": "x"})
	for range depth {
		chain = map[string]any{"not": chain}
	}
	vars := map[string]any{"w": chain}

	budget := maxInputPathElems
	var keys []InputKey
	got := allocatedBytes(func() {
		budget = maxInputPathElems
		keys, _ = inputKeys(types, typ, v, vars, &budget)
	})
	if len(keys) != depth+1 {
		t.Fatalf("%d keys, want %d", len(keys), depth+1)
	}
	// Doubling copies a chain at most twice over, so its paths cost under
	// four elements a level; a path cloned per key is depth/2 a level.
	if spent := maxInputPathElems - budget; spent > 4*(depth+1) {
		t.Fatalf("the chain's paths took %d elements, want at most %d", spent, 4*(depth+1))
	}
	if perKey := got / uint64(len(keys)); perKey > 1024 {
		t.Fatalf("the walk allocated %d bytes a key", perKey)
	}
}
