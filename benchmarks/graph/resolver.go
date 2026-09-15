package graph

import "github.com/syssam/graphql-go/benchmarks/internal/data"

// Resolver holds the in-memory dataset shared with the graphql-go bench.
type Resolver struct {
	Data []*data.User
}
