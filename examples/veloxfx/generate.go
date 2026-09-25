//go:build ignore

// Step one of go generate: velox writes the ORM into velox/ and one SDL file
// per entity into velox/schema/. Step two is gqlc, configured in gqlc.yaml.
package main

import (
	"log/slog"
	"os"

	"github.com/syssam/velox/compiler"
	"github.com/syssam/velox/compiler/gen"
	veloxgql "github.com/syssam/velox/contrib/graphql"
)

func main() {
	// velox keeps its schema loader binary in .velox/ and relinks it only when
	// `go build -n` prints a link step. With Go under a path containing a
	// space ("C:\Program Files\Go") the linker is printed quoted, its last
	// field is `link.exe"`, the check never matches, and the stale loader
	// generates from the old schema forever. Removing it costs ~1s.
	//
	// The output directory goes too. velox records what it wrote in
	// velox/.velox-manifest and deletes what a later run no longer writes,
	// but the GraphQL extension's files (query/gql_pagination_*.go,
	// entity/gql_edge_*.go) are not recorded, so an entity that stops being
	// a connection leaves a pagination file behind that no longer compiles.
	// Go's build cache keys on content, so rewriting unchanged files costs
	// no recompilation.
	for _, dir := range []string{".velox", "velox"} {
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("clear velox output", "dir", dir, "error", err)
			os.Exit(1)
		}
	}
	ex, err := veloxgql.NewExtension(
		veloxgql.WithSchemaGenerator(),
		veloxgql.WithSchemaPath("./velox/schema"),
		// One SDL file per entity, so gqlc emits one group, and one Resolver
		// interface, per entity.
		veloxgql.WithSchemaSplitMode(veloxgql.SchemaSplitPerEntity),
		// Node ids here are per-table integers, so node(id: 1) could be any
		// entity. examples/relaynode shows the global-id contract instead.
		veloxgql.WithRelaySpec(false),
	)
	if err != nil {
		slog.Error("velox graphql extension", "error", err)
		os.Exit(1)
	}
	cfg, err := gen.NewConfig(gen.WithTarget("./velox"))
	if err != nil {
		slog.Error("velox config", "error", err)
		os.Exit(1)
	}
	if err := compiler.Generate("./schema", cfg, compiler.Extensions(ex)); err != nil {
		slog.Error("velox", "error", err)
		os.Exit(1)
	}
}
