package graphql

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// docMu is one lock for the executor, and a plan cache miss hashes the query
// text under it. The design has called that "known and accepted" without ever
// measuring it, which is the kind of claim this benchmark exists to settle:
// every iteration here is a distinct query, so every one is a miss.
//
// Read it against BenchmarkDocumentHit, which is the same work with the lock
// uncontended. The gap is what a miss under concurrency actually costs.
func distinctQuery(n int, width int) string {
	var q strings.Builder
	q.WriteString("{")
	for i := range width {
		fmt.Fprintf(&q, " a%d_%d: users { id name nick tags role }", n, i)
	}
	q.WriteString(" }")
	return q.String()
}

func benchDocument(b *testing.B, width int, distinct bool) {
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s)
	ctx := context.Background()
	var seq atomic.Int64
	shared := distinctQuery(0, width)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q := shared
			if distinct {
				q = distinctQuery(int(seq.Add(1)), width)
			}
			resp := e.Execute(ctx, &Request{Query: q})
			if len(resp.Errors) > 0 {
				b.Fatalf("errors: %v", resp.Errors)
			}
			resp.Release()
		}
	})
}

// Every request is a distinct query, so every one misses the plan cache and
// takes docMu to hash its text.
func BenchmarkDocumentMiss(b *testing.B) { benchDocument(b, 24, true) }

// The same query every time: one miss, then hits. The control.
func BenchmarkDocumentHit(b *testing.B) { benchDocument(b, 24, false) }
