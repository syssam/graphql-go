// Package graph is gqlgen's generated binding for the benchmark schema. It
// exists so both engines answer the same queries over the same data.
package graph

import "github.com/syssam/graphql-go/benchmarks/internal/data"

// Resolver holds the in-memory dataset shared with the graphql-go bench.
type Resolver struct {
	Data []*data.User
}
