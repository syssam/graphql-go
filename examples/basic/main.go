// Command basic serves the example blog schema on http://localhost:8080/graphql.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/basic/schema"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	s, err := schema.NewSchema(schema.NewStore())
	if err != nil {
		slog.Error("building schema", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(graphql.NewExecutor(s)))

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

	slog.Info("serving GraphQL", "addr", *addr, "path", "/graphql")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}
