// Command server runs the veloxfx example:
//
//	go run ./cmd/server
//
// fx.Run blocks until SIGINT or SIGTERM and then runs every stop hook, so
// there is no signal handling here to get wrong.
package main

import (
	"flag"

	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dsn := flag.String("dsn", "file:veloxfx?mode=memory&cache=shared&_pragma=foreign_keys(1)", "sqlite data source")
	flag.Parse()

	fx.New(
		veloxfx.Module,
		fx.Supply(veloxfx.Config{Addr: *addr, DSN: *dsn}),
	).Run()
}
