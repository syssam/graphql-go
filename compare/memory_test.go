package compare_test

import (
	"runtime"
	"testing"

	"github.com/syssam/graphql-go/compare"
)

// Memory is the dimension where the compiled-plan design costs rather than
// saves: graphql-go binds and validates the whole type graph at NewSchema and
// holds it, where gqlgen did that work at code generation time. That decides
// how many replicas fit on a node, so it is measured rather than left implied.

// retainedHeap reports how much heap a value still holds after construction,
// measured across a forced collection so only live data counts. keep is used
// to stop the compiler and collector from discarding the subject.
func retainedHeap(tb testing.TB, build func() any) uint64 {
	tb.Helper()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)

	v := build()

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(v)

	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}

// TestSchemaMemoryFootprint reports the heap each engine's schema retains.
// It asserts nothing: the figure is machine- and schema-dependent, and a
// threshold here would be a flake. It exists so the number is recorded and
// reproducible rather than guessed at.
func TestSchemaMemoryFootprint(t *testing.T) {
	ours := retainedHeap(t, func() any {
		e, err := compare.NewGraphQLGo()
		if err != nil {
			t.Fatal(err)
		}
		return e
	})

	theirs := retainedHeap(t, func() any { return compare.NewGqlgen() })

	t.Logf("schema retained heap: graphql-go %.1f MB, gqlgen %.1f MB (%.1fx)",
		float64(ours)/(1<<20), float64(theirs)/(1<<20),
		float64(ours)/float64(max(theirs, 1)))
}

// BenchmarkSchemaMemory makes the same measurement available to benchstat,
// reporting retained bytes as a custom metric alongside build time.
func BenchmarkSchemaMemory(b *testing.B) {
	b.Run("GraphQLGo", func(b *testing.B) {
		var retained uint64
		b.ReportAllocs()
		for b.Loop() {
			retained = retainedHeap(b, func() any {
				e, err := compare.NewGraphQLGo()
				if err != nil {
					b.Fatal(err)
				}
				return e
			})
		}
		b.ReportMetric(float64(retained)/(1<<20), "MB/schema")
	})
	b.Run("Gqlgen", func(b *testing.B) {
		var retained uint64
		b.ReportAllocs()
		for b.Loop() {
			retained = retainedHeap(b, func() any { return compare.NewGqlgen() })
		}
		b.ReportMetric(float64(retained)/(1<<20), "MB/schema")
	})
}
