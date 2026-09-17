// Package jitter spreads durations so that connections opened together do
// not all expire together and reconnect in one storm.
package jitter

import (
	"math/rand/v2"
	"time"
)

// Spread returns d moved uniformly by up to 10% either way, the spread grpc
// applies to MaxConnectionAge. Zero and negative durations are returned
// unchanged.
func Spread(d time.Duration) time.Duration {
	span := int64(d) / 5
	if d <= 0 || span == 0 {
		return d
	}
	return d - time.Duration(span/2) + time.Duration(rand.Int64N(span+1))
}
