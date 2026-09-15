// Command fiber serves the example note-board schema through Fiber v3, on
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
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlfiber"
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

	app := fiber.New()

	// gqlfiber.New serves GET and POST itself and answers every other method
	// with its own GraphQL error envelope; registering it with Post alone
	// would let Fiber's router answer a DELETE with a bare 405 instead. All
	// routes every method to the handler so it stays the one place that
	// decides.
	app.All("/graphql", gqlfiber.New(exec))
	app.Post("/graphql/stream", gqlfiber.SSE(exec))

	// WS refuses a cross-origin upgrade unless WithOriginPatterns names it;
	// a request with no Origin header (curl, websocat, same-origin pages) is
	// always accepted. That default is left as-is here rather than loosened
	// with WithInsecureSkipOriginCheck, which would only teach a copy of
	// this example to disable the one browser-facing check it has.
	app.Get("/graphql/ws", gqlfiber.WS(exec))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		if err := app.ShutdownWithContext(context.Background()); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("serving GraphQL", "addr", *addr,
		"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
	if err := app.Listen(*addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}
