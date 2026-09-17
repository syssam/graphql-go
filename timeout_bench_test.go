package graphql

import (
	"testing"
	"time"
)

// BenchmarkOperationTimeoutOn is the price of the option when set: one timer
// context per request. BenchmarkFieldPathBare is the same query without it.
func BenchmarkOperationTimeoutOn(b *testing.B) {
	benchFieldPath(b, WithOperationTimeout(time.Minute))
}
