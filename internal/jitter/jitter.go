// Package jitter spreads durations so that connections opened together do
// not all expire together and reconnect in one storm.
package jitter

import (
	"math"
	"math/rand/v2"
	"time"
)

// Spread returns d moved uniformly within [d-d/10, d+d/10] (integer
// division), the spread grpc applies to MaxConnectionAge. Zero and negative
// durations are returned unchanged, and the result is clamped to
// math.MaxInt64 rather than wrapping, so the largest duration still means
// never.
func Spread(d time.Duration) time.Duration {
	half := int64(d) / 10
	if d <= 0 || half == 0 {
		return d
	}
	lo := int64(d) - half
	off := rand.Int64N(2*half + 1)
	if off > math.MaxInt64-lo {
		return math.MaxInt64
	}
	return time.Duration(lo + off)
}
