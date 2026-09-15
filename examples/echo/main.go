// Command echo serves the example note-board schema through Echo v5, on
// every transport:
//
//	/graphql         queries and mutations
//	/graphql/stream  Server-Sent Events
//	/graphql/ws      graphql-transport-ws
//
// Subscribe to noteCreated on either streaming endpoint, then run the
// createNote mutation against /graphql to see the event arrive.
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

	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlecho"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	s, err := newSchema(newStore())
	if err != nil {
		slog.Error("building schema", "error", err)
		os.Exit(1)
	}

	exec := graphql.NewExecutor(s)

	e := echo.New()

	// Each handler serves more than one method itself (gqlhttp and gqlsse
	// both accept GET and POST; gqlws answers any non-upgrade request with
	// its own 426) and rejects the rest with its own GraphQL-aware error, so
	// every route is registered for every method with Any: Echo's router
	// must never intercept a method the handler would have accepted, or
	// answer one it would have rejected with more than a bare 405. See
	// transport/equivalence_test.go's "Echo is registered the same way,
	// through e.Any, for the same reason".
	e.Any("/graphql", gqlecho.New(exec))
	e.Any("/graphql/stream", gqlecho.SSE(exec))
	// WS refuses a cross-origin upgrade unless the handler is configured
	// with origin patterns; a request with no Origin header (curl,
	// websocat, same-origin pages) is always accepted. gqlws.New's default
	// is left as-is here rather than loosened with
	// WithInsecureSkipOriginCheck, matching the Fiber example.
	e.Any("/graphql/ws", gqlecho.WS(exec))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           e,
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
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}
