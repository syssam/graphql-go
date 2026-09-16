// Command fiber serves the example note-board schema through Fiber v3, on
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
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlfiber"
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

	// BodyLimit is set alongside gqlfiber's own default of 1 MiB because the
	// two bound different things: fasthttp has read and decompressed the
	// whole body before a handler sees it, so gqlfiber's limit is a length
	// check on memory already spent, and BodyLimit (4 MiB if left alone) is
	// the only thing bounding the read from the socket.
	app := fiber.New(fiber.Config{BodyLimit: 1 << 20})

	// Each handler serves more than one method itself (gqlfiber's GraphQL
	// and SSE handlers both accept GET and POST; its WS handler answers any
	// non-upgrade request with its own 426) and rejects the rest with its
	// own GraphQL-aware error, so every route is registered for every
	// method with All: Fiber's router must never intercept a method the
	// handler would have accepted, or answer one it would have rejected
	// with more than a bare 405. Registering /graphql with Post alone, for
	// example, would let Fiber's router answer a DELETE with a bare 405
	// instead of the handler's envelope.
	app.All("/graphql", gqlfiber.New(exec))
	app.All("/graphql/stream", gqlfiber.SSE(exec))

	// WS refuses a cross-origin upgrade unless WithOriginPatterns names it;
	// a request with no Origin header (curl, websocat, same-origin pages) is
	// always accepted. That default is left as-is here rather than loosened
	// with WithInsecureSkipOriginCheck, which would only teach a copy of
	// this example to disable the one browser-facing check it has.
	app.All("/graphql/ws", gqlfiber.WS(exec))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// A bare context.Background() here would never force-close: Fiber
		// only forces a shutdown once the context's deadline passes, so an
		// open SSE or WebSocket subscription -- exactly what this example
		// exists to demonstrate -- would hold the process open forever.
		if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("serving GraphQL", "addr", *addr,
		"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
	if err := app.Listen(*addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		return err
	}
	return nil
}
