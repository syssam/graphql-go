// Package veloxfx serves a velox-backed schema through Echo, with every piece
// constructed and shut down by uber/fx.
//
// The ORM and the SDL come from one source, schema/. velox writes both, one
// SDL file per entity; gqlc then binds that SDL straight to velox's entity
// types, one group per entity, so no second model type exists and a resolver
// returns what the ORM returns:
//
//	schema/*.go --velox--> velox/ (ORM) + velox/schema/*.graphql
//	velox/schema/*.graphql --gqlc AutoBind--> graph/<entity>/ (Resolver, Bindings)
//
// Each entity is implemented in its own package, internal/<entity>, the way a
// gRPC server implements each service: its own Resolver, its own Module, and
// one line registering its bindings. Nothing here lists the entities' fields,
// and a new entity changes this file by one line.
//
// fx runs start hooks in dependency order and stop hooks in reverse: the
// database is migrated before the server listens, and the server has drained
// and shut down before the database closes.
package veloxfx

//go:generate go run generate.go

import (
	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/todo"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/user"
	"github.com/syssam/graphql-go/transport/drain"
)

// Config is what the process supplies; everything else is built from it.
type Config struct {
	// Addr is the listen address. ":0" picks a free port; Server.Addr
	// reports the one chosen.
	Addr string
	// DSN is a modernc.org/sqlite data source.
	DSN string
}

// Entities is one Module per entity package.
var Entities = fx.Options(
	todo.Module,
	user.Module,
)

// Module is the application, minus its Config.
var Module = fx.Module("veloxfx", Entities, app)

// app is everything but the entities.
var app = fx.Options(
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
