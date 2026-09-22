// Command subgraphs serves both federation subgraphs so a real router can be
// pointed at them:
//
//	/products/graphql
//	/reviews/graphql
//
// Two subgraphs in one process is not how they would be deployed -- that is
// the whole point of federation -- but it is one command to start, and a
// router cannot tell the difference: it reads each endpoint's _service and
// composes. Nothing in either subgraph knows the other exists.
//
// With rover and the Apollo Router:
//
//	rover supergraph compose --config supergraph.yaml > supergraph.graphql
//	router --supergraph supergraph.graphql
//
// supergraph.yaml is in this directory. The composition is what proves the
// two agree; federation_test.go plays the router for one query so that the
// contract is checked without either tool installed.
//
// The wiring here is deliberately bare. Limits, authorization, persisted
// queries, tracing and a shutdown drain belong on a subgraph too, and
// examples/storefront is where they are shown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/syssam/graphql-go/examples/federation"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

func main() {
	if err := run(); err != nil {
		slog.Error("subgraphs", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	products, err := federation.NewProducts()
	if err != nil {
		return fmt.Errorf("products subgraph: %w", err)
	}
	reviews, err := federation.NewReviews()
	if err != nil {
		return fmt.Errorf("reviews subgraph: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/products/graphql", gqlhttp.New(products))
	mux.Handle("/reviews/graphql", gqlhttp.New(reviews))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// No drain here: neither subgraph serves subscriptions, so every
		// request ends by itself and Shutdown is enough. A subgraph that did
		// would need transport/drain, as examples/storefront shows.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("serving subgraphs", "addr", *addr,
		"products", "/products/graphql", "reviews", "/reviews/graphql")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}
