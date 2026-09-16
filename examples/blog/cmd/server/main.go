// Command basic serves the example blog schema on every transport:
//
//	/graphql         queries and mutations over HTTP
//	/graphql/stream  Server-Sent Events
//	/graphql/ws      graphql-transport-ws
//
// Subscribe to postCreated on either streaming endpoint, then run the
// createPost mutation against /graphql to see the event arrive.
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
	blog "github.com/syssam/graphql-go/examples/blog"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
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

	s, err := blog.NewSchema()
	if err != nil {
		return fmt.Errorf("building schema: %w", err)
	}

	exec := graphql.NewExecutor(s)
	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(exec))
	mux.Handle("/graphql/stream", gqlsse.New(exec))
	mux.Handle("/graphql/ws", gqlws.New(exec))

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

	slog.Info("serving GraphQL", "addr", *addr,
		"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
