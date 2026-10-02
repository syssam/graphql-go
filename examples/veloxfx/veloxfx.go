// Package veloxfx serves a velox-backed schema through Echo, with every piece
// constructed and shut down by uber/fx.
//
// The ORM and the SDL come from one source, schema/. velox writes both, one
// SDL file per entity; gqlc then binds that SDL straight to velox's entity
// types, one group per entity, so no second model type exists and a resolver
// returns what the ORM returns:
//
//	schema/*.go --velox (generate.go)--> velox/ (ORM) + velox/schema/*.graphql
//	velox/schema/*.graphql + sdl/*.graphql --gqlc (gqlc.yaml)--> graph/
//
// The layers are those of a gRPC server, one package per entity in each:
//
//	graph/<entity>            generated.go: bindings, args, Resolver interface
//	                          <entity>.resolvers.go: the Handler, thin, scaffolded
//	graph/model/<entity>      generated: types the SDL declares and Go lacks
//	internal/service/<entity> written: the rules
//
// An edit to one entity recompiles that entity's packages. GUIDE.md walks
// through adding one.
//
// fx runs start hooks in dependency order and stop hooks in reverse: the
// database is migrated before the server listens, and the server has drained
// and shut down before the database closes.
package veloxfx

//go:generate go run generate.go
//go:generate go tool gqlc -config gqlc.yaml

import (
	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/service"
	"github.com/syssam/graphql-go/transport/drain"
)

// Config is what the process supplies; everything else is built from it.
type Config struct {
	// Addr is the listen address. ":0" picks a free port; Server.Addr
	// reports the one chosen.
	Addr string
	// Driver is "sqlite" (modernc.org/sqlite, the default) or "postgres"
	// (github.com/lib/pq).
	Driver string
	// DSN is the driver's data source.
	DSN string
	// MaxConns bounds the database connections the replica holds, open and
	// idle alike; zero is 20. See configurePool.
	MaxConns int
}

// Module is the application, minus its Config: the services, the GraphQL
// layer in front of them, and the server.
var Module = fx.Module("veloxfx", service.Module, Resolvers, appCore)

// appCore is everything but the services and the resolvers.
var appCore = fx.Options(
	fx.Provide(
		NewClient,
		NewSchema,
		NewExecutor,
		drain.New,
		NewEcho,
		NewServer,
	),
	// Nothing depends on *Server, and fx builds only what is asked for.
	fx.Invoke(func(*Server) {}),
)
