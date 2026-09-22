package graphql

import (
	"fmt"
	"strings"
	"testing"
)

// unboundSchema is a schema whose object types have no bindings at all, so
// coverage reports one error per type. Forty is past the point where a reader
// scrolls, which is where the ordering starts to matter.
func unboundSchema(n int) string {
	var sdl strings.Builder
	sdl.WriteString("type Query { a: T0 }\n")
	for i := range n {
		fmt.Fprintf(&sdl, "type T%d { id: ID! ref: T%d }\n", i, (i+1)%n)
	}
	return sdl.String()
}

// Build errors must come out in the same order every time.
//
// They did not: several of the checks range over b.ast.Types, which is a map,
// so a schema with forty unbound types printed them in a different order on
// every run. At the scale this library is for -- thousands of types, bound a
// package at a time -- that means two runs cannot be diffed, and there is no
// way to tell a fix from a reshuffle.
func TestBuildErrorsAreOrdered(t *testing.T) {
	sdl := unboundSchema(40)

	first := ""
	for i := range 8 {
		_, err := NewSchema(SDL(sdl))
		if err == nil {
			t.Fatal("expected binding errors")
		}
		got := err.Error()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			// Report the first line that differs; the whole text is too long
			// to read in a failure.
			a, b := strings.Split(first, "\n"), strings.Split(got, "\n")
			for j := range min(len(a), len(b)) {
				if a[j] != b[j] {
					t.Fatalf("run %d differs at line %d:\n  first: %s\n  now:   %s", i, j, a[j], b[j])
				}
			}
			t.Fatalf("run %d differs in length: %d lines against %d", i, len(b), len(a))
		}
	}

	// And the order is one a reader can navigate: sorted within the phase, so
	// a category is contiguous and a name is findable.
	lines := strings.Split(first, "\n")
	if len(lines) != 41 {
		t.Fatalf("got %d errors, want one per unbound type: the 40 plus Query (41)", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("line %d is out of order:\n  %s\n  %s", i, lines[i-1], lines[i])
		}
	}
}
