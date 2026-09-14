//go:build tools

// Package benchmarks pins the gqlgen codegen packages so buildbench can drive
// them offline from a pinned go.sum.
package benchmarks

import (
	_ "github.com/99designs/gqlgen/api"
	_ "github.com/99designs/gqlgen/codegen/config"
)
