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
	ex, err := veloxgql.NewExtension(
		veloxgql.WithSchemaGenerator(),
		veloxgql.WithSchemaPath("./velox/schema"),
		// One SDL file per entity, so gqlc emits one group, and one Resolver
		// interface, per entity.
		veloxgql.WithSchemaSplitMode(veloxgql.SchemaSplitPerEntity),
		// Node ids here are per-table integers, so node(id: 1) could be any
		// entity. examples/relaynode shows the global-id contract instead.
		veloxgql.WithRelaySpec(false),
		veloxgql.WithRelayConnection(false),
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
