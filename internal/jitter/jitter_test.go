package jitter

import (
	"math"
	"testing"
	"time"
)

func TestSpreadStaysWithinTenPercent(t *testing.T) {
	const d = time.Hour
	lo, hi := d-d/10, d+d/10
	seen := map[time.Duration]bool{}
	for range 10_000 {
		got := Spread(d)
		if got < lo || got > hi {
			t.Fatalf("Spread(%v) = %v, outside [%v, %v]", d, got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct values in 10000 samples; the jitter is not spreading", len(seen))
	}
}

func TestSpreadLeavesNonPositiveAlone(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := Spread(d); got != d {
			t.Fatalf("Spread(%v) = %v, want it unchanged", d, got)
		}
	}
}

// TestSpreadDoesNotOverflow: a limit set to the largest duration means never,
// and a wrapped negative result would make a timer fire at once.
func TestSpreadDoesNotOverflow(t *testing.T) {
	d := time.Duration(math.MaxInt64)
	lo := d - d/10
	for range 10_000 {
		if got := Spread(d); got < lo {
			t.Fatalf("Spread(MaxInt64) = %v, want at least %v", got, lo)
		}
	}
}
