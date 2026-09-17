package graphql

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

const limitSDL = `
type Item { body: String! }
type Section { items: [Item!]! }
type Row { text: String! next: String! }
type Query {
  short: String!
  failNullable: String
  a: Section!
  b: Section!
  rows: [Row!]!
}
type Subscription { section: Section! }
`

type limitItem struct{ n int }
type limitSection struct{ n int }
type limitRow struct{ n int }

// limitFixture counts how many item bodies were written, which is how a test
// tells an execution that stopped early from one that wrote everything and was
// only rejected at the end.
type limitFixture struct {
	bodies   atomic.Int64
	nexts    atomic.Int64
	sections chan *limitSection
}

func newLimitExecutor(t *testing.T, opts ...ExecutorOption) (*limitFixture, *Executor) {
	t.Helper()
	f := &limitFixture{sections: make(chan *limitSection)}
	items := make([]*limitItem, 1000)
	for i := range items {
		items[i] = &limitItem{n: i}
	}
	rows := make([]*limitRow, 1000)
	for i := range rows {
		rows[i] = &limitRow{n: i}
	}
	body := strings.Repeat("x", 100)
	s, err := NewSchema(SDL(limitSDL),
		Object[limitItem]("Item",
			Field("body", func(*limitItem) string {
				f.bodies.Add(1)
				return body
			}),
		),
		Object[limitSection]("Section",
			Field("items", func(*limitSection) []*limitItem { return items }),
		),
		Object[limitRow]("Row",
			Field("text", func(*limitRow) string { return body }),
			// next is a resolver, so a list of rows is written one goroutine per
			// element, and text before it gives each element a checkpoint that
			// reports its bytes before next resolves.
			Resolve("next", func(context.Context, *limitRow) (string, error) {
				f.nexts.Add(1)
				return "n", nil
			}),
		),
		Query(
			Field("short", func(Root) string { return "abc" }),
			Resolve("failNullable", func(context.Context, Root) (*string, error) { return nil, errors.New("boom") }),
			Resolve("a", func(context.Context, Root) (*limitSection, error) { return &limitSection{}, nil }),
			Resolve("b", func(context.Context, Root) (*limitSection, error) { return &limitSection{}, nil }),
			Field("rows", func(Root) []*limitRow { return rows }),
		),
		Subscription(
			Subscribe("section", func(context.Context) (<-chan *limitSection, error) { return f.sections, nil }),
		),
	)
	if err != nil {
		t.Fatalf("limit schema: %v", err)
	}
	return f, NewExecutor(s, opts...)
}

func expectTooLarge(t *testing.T, resp *Response, limit int64) {
	t.Helper()
	if got := string(resp.Data); got != "null" {
		t.Fatalf("data = %.200s, want null", got)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("want exactly one error, got %s", errorsJSON(resp.Errors))
	}
	err := resp.Errors[0]
	if err.Extensions["code"] != CodeResponseTooLarge {
		t.Errorf("code = %v, want %s", err.Extensions["code"], CodeResponseTooLarge)
	}
	if want := "response exceeds the maximum size of " + itoa(limit) + " bytes"; err.Message != want {
		t.Errorf("message = %q, want %q", err.Message, want)
	}
	if err.Path != nil || len(err.Locations) != 0 {
		t.Errorf("a response-level error has path %v and locations %v", err.Path, err.Locations)
	}
}

// TestResponseLimitExactBoundary pins that the final decision is exact, not
// the approximate in-flight count: {"short":"abc"} is 15 bytes.
func TestResponseLimitExactBoundary(t *testing.T) {
	_, fits := newLimitExecutor(t, WithMaxResponseBytes(15))
	expectData(t, run(t, fits, `{ short }`, ""), `{"short":"abc"}`)

	_, over := newLimitExecutor(t, WithMaxResponseBytes(14))
	expectTooLarge(t, run(t, over, `{ short }`, ""), 14)
}

// TestResponseLimitReplacesErrors pins the failure shape: the field error from
// failNullable points into data that no longer exists, so it must not survive.
func TestResponseLimitReplacesErrors(t *testing.T) {
	_, e := newLimitExecutor(t, WithMaxConcurrency(0), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ short failNullable a { items { body } } }`, ""), 8<<10)
}

// TestResponseLimitStopsSequentialExecution is the difference between a limit
// and a post-hoc check: the 1,000 items would be written in full by a check
// that only looked at the finished response.
func TestResponseLimitStopsSequentialExecution(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(0), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ a { items { body } } }`, ""), 8<<10)
	if n := f.bodies.Load(); n >= 500 {
		t.Fatalf("wrote %d of 1000 bodies under an 8 KiB limit; execution did not stop early", n)
	}
}

// TestResponseLimitStopsSubWriters covers the concurrent path, where a and b
// each write into their own sub-writer and nothing reaches the root writer
// until both finish. Without the shared budget both write all 1,000 items.
func TestResponseLimitStopsSubWriters(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(4), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ a { items { body } } b { items { body } } }`, ""), 8<<10)
	if n := f.bodies.Load(); n >= 1000 {
		t.Fatalf("wrote %d of 2000 bodies under an 8 KiB limit; sub-writers did not share the budget", n)
	}
}

// TestResponseLimitSplicedBytesCountOnce is the exact boundary on the
// concurrent path: short follows two concurrent fields, so its checkpoint runs
// after both sub-writers are spliced in. Counting them at the splice as well as
// in the sub-writers rejected this response at half its size.
func TestResponseLimitSplicedBytesCountOnce(t *testing.T) {
	const q = `{ a { items { body } } b { items { body } } short }`
	_, unlimited := newLimitExecutor(t, WithMaxConcurrency(4))
	want := run(t, unlimited, q, "")
	if len(want.Errors) != 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(want.Errors))
	}
	_, e := newLimitExecutor(t, WithMaxConcurrency(4), WithMaxResponseBytes(int64(len(want.Data))))
	expectData(t, run(t, e, q, ""), string(want.Data))
}

// TestResponseLimitUnsetWritesEverything guards the test above against passing
// for the wrong reason: with no limit the same query really writes every body.
func TestResponseLimitUnsetWritesEverything(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(4))
	resp := run(t, e, `{ a { items { body } } b { items { body } } }`, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	if n := f.bodies.Load(); n != 2000 {
		t.Fatalf("wrote %d bodies with no limit, want 2000", n)
	}

	resp2 := run(t, e, `{ rows { text next } }`, "")
	if len(resp2.Errors) != 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp2.Errors))
	}
	if n := f.nexts.Load(); n != 1000 {
		t.Fatalf("resolved %d rows with no limit, want 1000", n)
	}
}

// TestResponseLimitStopsConcurrentList covers list elements written one
// goroutine each, the case the limit exists for. Without the shared budget
// every element resolves next.
func TestResponseLimitStopsConcurrentList(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(4), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ rows { text next } }`, ""), 8<<10)
	if n := f.nexts.Load(); n >= 500 {
		t.Fatalf("resolved %d of 1000 rows under an 8 KiB limit; list elements did not share the budget", n)
	}
}

// TestResponseLimitStopsSubscriptionEvent pins that an event's writer carries
// the limit while it executes, not only when its finished size is checked.
func TestResponseLimitStopsSubscriptionEvent(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxResponseBytes(8<<10))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { section { items { body } } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		f.sections <- &limitSection{}
		close(f.sections)
	}()

	resp := nextResponse(t, out)
	expectTooLarge(t, resp, 8<<10)
	resp.Release()
	if n := f.bodies.Load(); n >= 500 {
		t.Fatalf("wrote %d of 1000 bodies in one event under an 8 KiB limit; the event did not stop early", n)
	}
	expectClosed(t, out)
}

// TestResponseLimitPerSubscriptionEvent pins that the limit is per event: one
// oversized event fails alone and the stream carries on.
func TestResponseLimitPerSubscriptionEvent(t *testing.T) {
	src, e := newSubExecutor(t, WithMaxResponseBytes(64))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id body } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.messages <- &subMessage{ID: "1", Body: strings.Repeat("x", 100)}
		src.messages <- &subMessage{ID: "2", Body: "ok"}
		close(src.messages)
	}()

	first := nextResponse(t, out)
	expectTooLarge(t, first, 64)
	first.Release()

	second := nextResponse(t, out)
	expectData(t, second, `{"messages":{"id":"2","body":"ok"}}`)
	second.Release()
	expectClosed(t, out)
}

// TestExecutorResponseLimitDefault pins the default. It was set only after an
// enabled limit measured no cost distinguishable from none; a change to the
// write path that makes the limit expensive should revisit it, not drop it
// silently.
func TestExecutorResponseLimitDefault(t *testing.T) {
	_, base := newFixtureExecutor(t)
	if got := NewExecutor(base.Schema()).maxResponseBytes; got != 64<<20 {
		t.Fatalf("default maxResponseBytes = %d, want 64 MiB", got)
	}
	if got := NewExecutor(base.Schema(), WithMaxResponseBytes(0)).maxResponseBytes; got != 0 {
		t.Fatalf("WithMaxResponseBytes(0) = %d, want 0 (unlimited)", got)
	}
}
