// Command server runs the backoffice example:
//
//	go run ./cmd/server
//
// fx.Run blocks until SIGINT or SIGTERM and then runs every stop hook, so
// there is no signal handling here to get wrong.
package main

import (
	"flag"

	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/backoffice"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	driver := flag.String("driver", "sqlite", "database driver: sqlite or postgres")
	dsn := flag.String("dsn", "file:backoffice?mode=memory&cache=shared&_pragma=foreign_keys(1)", "data source, e.g. postgres://user:pass@localhost/backoffice?sslmode=disable")
	flag.Parse()

	fx.New(
		backoffice.Module,
		fx.Supply(backoffice.Config{Addr: *addr, Driver: *driver, DSN: *dsn}),
	).Run()
}
