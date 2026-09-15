// Command echo serves the example note-board schema through Echo v5, on
// every transport:
//
//	POST /graphql         queries and mutations
//	POST /graphql/stream  Server-Sent Events
//	GET  /graphql/ws      graphql-transport-ws
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
	e.POST("/graphql", gqlecho.New(exec))
	e.POST("/graphql/stream", gqlecho.SSE(exec))
	e.GET("/graphql/ws", gqlecho.WS(exec))

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
