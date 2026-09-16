package graphql

import (
	"iter"
	"strconv"
	"strings"
	"testing"
)

// lazyListSize is the number of elements the lazy list benchmarks generate.
// It needs to be large enough that a materialized []*lazyItem backing array
// is not lost in the noise every benchmark iteration also pays for (schema
// lookup, plan cache hit, JSON encoding); 1000 elements is comfortably past
// that point while each iteration still runs in well under a millisecond.
const lazyListSize = 1000

type lazyItem struct {
	ID   int
	Name string
}

// newLazyItem is the per-element work both benchmark resolvers do, so the
// backing array is the only structural difference between them.
func newLazyItem(i int) *lazyItem {
	return &lazyItem{ID: i, Name: "item-" + strconv.Itoa(i)}
}

// newLazySeqExecutor builds a schema whose LazyItem type has only pure
// Field bindings, so {lazySlice{...}} and {lazySeq{...}} both take
// writeList's sequential path rather than writeListConcurrent, which drains
// a seq into a []any before writing (by design, to get an exact count for
// DataLoader batching) and would measure nothing about laziness.
// lazySlice materializes all lazyListSize elements into a []*lazyItem before
// returning; lazySeq generates and yields one element at a time and never
// holds a backing array.
func newLazySeqExecutor(tb testing.TB) *Executor {
	tb.Helper()
	s, err := NewSchema(SDL(`
		type LazyItem { id: Int! name: String! }
		type Query {
			lazySlice: [LazyItem!]!
			lazySeq: [LazyItem!]!
		}
	`),
		Object[lazyItem]("LazyItem",
			Field("id", func(v *lazyItem) int { return v.ID }),
			Field("name", func(v *lazyItem) string { return v.Name }),
		),
		Query(
			Field("lazySlice", func(Root) []*lazyItem {
				out := make([]*lazyItem, lazyListSize)
				for i := range out {
					out[i] = newLazyItem(i)
				}
				return out
			}),
			Field("lazySeq", func(Root) iter.Seq[*lazyItem] {
				return func(yield func(*lazyItem) bool) {
					for i := 0; i < lazyListSize; i++ {
						if !yield(newLazyItem(i)) {
							return
						}
					}
				}
			}),
		),
	)
	if err != nil {
		tb.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

// The slice and seq resolvers must be indistinguishable in the response,
// same as the fixture's users/usersSeq pair, or the benchmark below would be
// comparing apples to oranges.
func TestLazySeqMatchesLazySlice(t *testing.T) {
	e := newLazySeqExecutor(t)
	slice := run(t, e, `{lazySlice{id name}}`, "")
	seq := run(t, e, `{lazySeq{id name}}`, "")
	if len(slice.Errors) != 0 || len(seq.Errors) != 0 {
		t.Fatalf("errors: slice=%v seq=%v", slice.Errors, seq.Errors)
	}
	want := strings.Replace(string(slice.Data), `"lazySlice"`, `"lazySeq"`, 1)
	if got := string(seq.Data); got != want {
		t.Fatalf("seq list = %s, want %s", got, want)
	}
}

// TestLazySeqBenchmarkUsesSequentialPath confirms {lazySeq{id name}} takes
// writeList's sequential path rather than writeListConcurrent, since that is
// the only path where a seq's laziness can save anything (the concurrent
// path fully drains a seq into a []any up front regardless). It forces a
// non-null element to fail partway through and counts how many elements the
// generator produced: the sequential path's writeElem returns false on the
// first failed write, which the range-over-func plumbing turns into an early
// return from the generator; the concurrent path would already have drained
// every one of the lazyListSize elements before noticing the failure.
func TestLazySeqBenchmarkUsesSequentialPath(t *testing.T) {
	const failAt = 5
	generated := 0
	s, err := NewSchema(SDL(`
		type LazyItem { id: Int! name: String! }
		type Query { lazySeq: [LazyItem!]! }
	`),
		Object[lazyItem]("LazyItem",
			Field("id", func(v *lazyItem) int { return v.ID }),
			Field("name", func(v *lazyItem) string { return v.Name }),
		),
		Query(
			Field("lazySeq", func(Root) iter.Seq[*lazyItem] {
				return func(yield func(*lazyItem) bool) {
					for i := 0; i < lazyListSize; i++ {
						generated++
						item := newLazyItem(i)
						if i == failAt {
							item = nil // nil in a non-null element position fails the write
						}
						if !yield(item) {
							return
						}
					}
				}
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := run(t, NewExecutor(s), `{lazySeq{id name}}`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("want an error for a null element in a non-null position")
	}
	if generated > failAt+2 {
		t.Fatalf("generator produced %d elements after failing at index %d; the concurrent path would drain all %d, the sequential path should stop within a couple", generated, failAt, lazyListSize)
	}
}

// BenchmarkLazySeqSlice and BenchmarkLazySeqLazy are a matched pair testing
// the iter.Seq design doc's actual claim: a resolver that "stops building a
// slice it only ever hands to the writer once". Both generate the same
// lazyListSize elements with the same per-element work (newLazyItem); the
// only structural difference is whether a []*lazyItem backing array exists.
// This is unlike BenchmarkListResultsSeq's usersSeq, which already holds a
// slice and only wraps it in a yield loop — that measures wrapping overhead,
// this measures the materialization cost the feature is meant to avoid.
func BenchmarkLazySeqSlice(b *testing.B) {
	e := newLazySeqExecutor(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := run(b, e, `{lazySlice{id name}}`, "")
		resp.Release()
	}
}

func BenchmarkLazySeqLazy(b *testing.B) {
	e := newLazySeqExecutor(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := run(b, e, `{lazySeq{id name}}`, "")
		resp.Release()
	}
}
