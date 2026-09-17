package jitter

import (
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
