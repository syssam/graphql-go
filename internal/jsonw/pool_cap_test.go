package jsonw

import "testing"

// grow appends until the buffer holds at least n bytes, the way a response is
// written: many small appends rather than one sized allocation.
func grow(w *Writer, n int) {
	chunk := make([]byte, 4096)
	for len(w.buf) < n {
		w.buf = append(w.buf, chunk...)
	}
}

// append overshoots, so the capacity a response reaches is larger than the
// response. That is why the pooling cliff is well below maxPooledCap and why
// reasoning about it in terms of response size is wrong: at a 4 MiB cap a
// 3.62 MB response was already being dropped.
//
// Pinned because the growth factor is the runtime's, not ours. If it changes,
// the numbers in docs/performance.md stop describing this code and this test
// is where that surfaces.
func TestBufferCapacityOvershootsTheResponse(t *testing.T) {
	w := New()
	grow(w, 3_800_000)
	if cap(w.buf) <= len(w.buf) {
		t.Fatalf("capacity %d did not overshoot a %d-byte response", cap(w.buf), len(w.buf))
	}
	if over := float64(cap(w.buf)) / float64(len(w.buf)); over > 1.5 {
		t.Errorf("capacity overshoots by %.2fx; docs/performance.md assumes well under 1.5", over)
	}
}

// The cap has to cover what a large schema's introspection actually reaches,
// or that query -- usually unauthenticated, and polled by GraphiQL, Apollo
// Studio and codegen tools -- pays twice the bytes on every request.
//
// The length here is measured, not extrapolated. The previous version of this
// test used 4.3 MB, derived from "5 455 types at the 0.79 KB/type the
// synthetic schema showed". The real schema is 5 516 types and introspects to
// 6.56 MB, because its types are about twice as wide; that response reaches a
// capacity of 8.05 MiB and the 8 MiB cap dropped it on every request. Type
// count does not predict the response, so this test pins the response.
func TestPoolCapCoversALargeSchemasIntrospection(t *testing.T) {
	// nextapp-graphql, 5 516 types: len(resp.Data) == 6.56 MiB.
	const realIntrospection = 6_878_658

	w := New()
	grow(w, realIntrospection)
	if cap(w.buf) > maxPooledCap {
		t.Errorf("a %d-byte response reached capacity %d, above maxPooledCap %d; "+
			"that response stops being pooled and costs about twice the bytes per request",
			len(w.buf), cap(w.buf), maxPooledCap)
	}
}
