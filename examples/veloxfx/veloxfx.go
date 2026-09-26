// Package veloxfx serves a velox-backed schema through Echo, with every piece
// constructed and shut down by uber/fx.
//
// The ORM and the SDL come from one source, schema/. velox writes both, one
// SDL file per entity; gqlc then binds that SDL straight to velox's entity
// types, one group per entity, so no second model type exists and a resolver
// returns what the ORM returns:
//
//	schema/*.go --velox (generate.go)--> velox/ (ORM) + velox/schema/*.graphql
//	velox/schema/*.graphql + sdl/*.graphql --gqlc (gqlc.yaml)--> graph/<entity>/
//
// Entities are implemented by domain, the way a gRPC server package implements
// services: internal/catalog, internal/sales and internal/inventory each hold a
// Resolver per entity group they own and one Module registering them. Nothing
// here lists entities; a new entity changes its domain, and a new domain
// changes Domains by one line. GUIDE.md walks through adding one.
//
// fx runs start hooks in dependency order and stop hooks in reverse: the
// database is migrated before the server listens, and the server has drained
// and shut down before the database closes.
package veloxfx

//go:generate go run generate.go
//go:generate go tool gqlc -config gqlc.yaml

import (
	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/catalog"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/inventory"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/sales"
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

// Domains is one Module per domain package. Each registers the groups it
// implements; this list is all that grows when a domain is added.
var Domains = fx.Options(
	catalog.Module,
	sales.Module,
	inventory.Module,
)

// Module is the application, minus its Config.
var Module = fx.Module("veloxfx", Domains, appCore)

// appCore is everything but the domains.
var appCore = fx.Options(
	fx.Provide(
		NewClient,
		NewSchema,
		NewExecutor,
		drain.New,
		NewEcho,
		NewServer,
	),
	// Order reads are narrowed to the viewer's own, on whichever client the
	// app ends up with -- a test decorating it keeps the filter.
	fx.Invoke(ownOrders),
	// Nothing depends on *Server, and fx builds only what is asked for.
	fx.Invoke(func(*Server) {}),
)
