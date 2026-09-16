// Command quickstart is the smallest thing in this repository that runs: one
// SDL file, hand-written bindings, no code generation and no layers.
//
//	/graphql         queries and mutations over HTTP
//	/graphql/stream  Server-Sent Events
//
// Subscribe to noteCreated on the streaming endpoint, then run the createNote
// mutation against /graphql to see the event arrive. For what this looks like
// once a service grows a database and a wire format, read examples/blog.
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

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}

// run holds every deferred cleanup, so that main can exit non-zero without
// skipping any of it.
func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	s, err := newSchema(newStore())
	if err != nil {
		return fmt.Errorf("building schema: %w", err)
	}

	exec := graphql.NewExecutor(s)
	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(exec))
	mux.Handle("/graphql/stream", gqlsse.New(exec))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("serving GraphQL", "addr", *addr, "http", "/graphql", "sse", "/graphql/stream")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
