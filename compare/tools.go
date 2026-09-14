//go:build tools

// Package compare pins the code generators it drives, so the module resolves
// them without a separate install step.
package compare

import (
	_ "github.com/99designs/gqlgen/api"
	_ "github.com/99designs/gqlgen/codegen/config"
	_ "github.com/syssam/graphql-go/benchmarks/buildbench"
	_ "github.com/syssam/graphql-go/codegen"
)
