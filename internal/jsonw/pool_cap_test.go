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
// 4 800 types introspect to 3.77 MB, which reaches about 4.2 MB of capacity.
// The consumer driving this library has 5 455 types, so roughly 5 MB. Six is
// the floor with a little room; the shipped value is 8 MiB.
func TestPoolCapCoversALargeSchemasIntrospection(t *testing.T) {
	const needed = 6 << 20
	if maxPooledCap < needed {
		t.Errorf("maxPooledCap is %d, below the %d a large schema's introspection reaches; "+
			"that response stops being pooled and costs about twice the bytes per request",
			maxPooledCap, needed)
	}

	// And the writer that held such a response is in fact poolable.
	w := New()
	grow(w, 4_300_000)
	if cap(w.buf) > maxPooledCap {
		t.Errorf("a %d-byte response reached capacity %d, above maxPooledCap %d",
			len(w.buf), cap(w.buf), maxPooledCap)
	}
}
