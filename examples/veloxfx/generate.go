//go:build ignore

// Generation is two steps, and the second reads what the first wrote:
//
//  1. velox writes the ORM into velox/ and the SDL into velox/schema.graphql.
//  2. gqlc binds that SDL to the ORM's own entity types, so graph/ holds
//     bindings and a Resolver interface but no second copy of User or Todo.
//
// Run it with go generate (see app.go).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/syssam/graphql-go/codegen"
	"github.com/syssam/velox/compiler"
	"github.com/syssam/velox/compiler/gen"
	veloxgql "github.com/syssam/velox/contrib/graphql"
)

func main() {
	if err := run(); err != nil {
		slog.Error("generate", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ex, err := veloxgql.NewExtension(
		veloxgql.WithSchemaGenerator(),
		veloxgql.WithSchemaPath("./velox/schema"),
		// One SDL file per entity, so gqlc emits one group, and one Resolver
		// interface, per entity.
		veloxgql.WithSchemaSplitMode(veloxgql.SchemaSplitPerEntity),
		// Node ids here are per-table integers, so node(id: 1) could be a user
		// or a todo. examples/relaynode shows the global-id contract instead.
		veloxgql.WithRelaySpec(false),
		veloxgql.WithRelayConnection(false),
	)
	if err != nil {
		return fmt.Errorf("velox graphql extension: %w", err)
	}
	cfg, err := gen.NewConfig(gen.WithTarget("./velox"))
	if err != nil {
		return fmt.Errorf("velox config: %w", err)
	}
	if err := compiler.Generate("./schema", cfg, compiler.Extensions(ex)); err != nil {
		return fmt.Errorf("velox: %w", err)
	}

	return codegen.Generate(context.Background(), codegen.Config{
		SchemaGlobs: []string{"velox/schema/*.graphql"},
		Output:      "graph",
		Package:     "github.com/syssam/graphql-go/examples/veloxfx/graph",
		Models:      map[string]string{"Time": "time.Time"},
		// velox writes @goModel on every type it owns; reading it is what
		// keeps the SDL the only place those bindings are stated.
		ModelDirective: codegen.ModelDirective{Name: "goModel", Arg: "model"},
		FieldDirective: codegen.FieldDirective{Name: "goField", ForceResolverArg: "forceResolver"},
		AutoBind:       []string{"./velox/entity"},
		// velox_todo.graphql is the todo group; velox's shared file, which
		// declares the roots and the common scalars, is root.
		GroupFunc: func(_, file string) string {
			stem := strings.TrimSuffix(filepath.Base(file), ".graphql")
			if stem == "schema" {
				return "root"
			}
			return strings.TrimPrefix(stem, "velox_")
		},
		// velox declares every root field in the shared file, so grouping by
		// file would put all of them in root. Following the returned type puts
		// createTodo beside Todo, which is where its author will look.
		RootFieldGroup: func(f codegen.RootField) string { return f.ReturnGroup },
		Notef: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "gqlc: "+format+"\n", args...)
		},
	})
}
