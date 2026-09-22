// Command server serves the Relay example on /graphql, so the global-id round
// trip can be run against a real endpoint:
//
//	# a connection, which hands back global ids and a cursor
//	curl -s localhost:8080/graphql -H content-type:application/json \
//	  -d '{"query":"{ users(first:1){ edges{ cursor node{ id name } } pageInfo{ hasNextPage endCursor } } }"}'
//
//	# the id from that response, handed straight back to node(id:)
//	curl -s localhost:8080/graphql -H content-type:application/json \
//	  -d '{"query":"query($id:ID!){ node(id:$id){ __typename ... on User { name } } }","variables":{"id":"VXNlcjox"}}'
//
// The wiring is bare on purpose. Limits, authorization, tracing and a shutdown
// drain belong on a Relay server too; examples/storefront is where they are
// shown, and docs/operations.md says which to turn on.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/relaynode"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	s, _, err := relaynode.New()
	if err != nil {
		return fmt.Errorf("building schema: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(graphql.NewExecutor(s)))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("serving GraphQL", "addr", *addr, "http", "/graphql",
		"try", `{ users(first:1){ edges{ node{ id name } } } }`)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
