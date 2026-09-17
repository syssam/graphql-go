// Command echo serves the examples/blog schema through Echo v5, on every
// transport:
//
//	/graphql         queries and mutations
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

	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// timeout is used both for ReadHeaderTimeout (Slowloris mitigation) and for
// how long shutdown waits for in-flight requests, matching the other example
// servers.
const timeout = 5 * time.Second

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
	// gqlecho.SSE and WS take gqlsse and gqlws options, so the drain is wired
	// the same way as on net/http.
	d := drain.New()
	e.Any("/graphql/stream", gqlecho.SSE(exec, gqlsse.WithDrain(d)))
	// WS refuses a cross-origin upgrade unless the handler is configured
	// with origin patterns; a request with no Origin header (curl,
	// websocat, same-origin pages) is always accepted. gqlws.New's default
	// is left as-is here rather than loosened with
	// WithInsecureSkipOriginCheck, matching the Fiber example.
	e.Any("/graphql/ws", gqlecho.WS(exec, gqlws.WithDrain(d)))

	// e.Start(*addr) is Echo's own quickstart method, but its doc comment
	// says it is "created for use in examples/demos and is deliberately
	// simple without providing configuration options" -- it exposes no way
	// to set ReadHeaderTimeout. StartConfig is Echo's documented answer for
	// that: it still builds a plain http.Server internally (this is a
	// startup/shutdown ergonomics wrapper, not a different transport, so it
	// changes nothing about request-serving performance), but it replaces
	// the hand-rolled signal/shutdown goroutine every other net/http-based
	// example here needs with two struct fields.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// StartConfig begins its graceful shutdown when ctx ends; the drain starts
	// at the same moment, because that shutdown waits for SSE streams only the
	// drain can end.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := d.Shutdown(drainCtx); err != nil {
			slog.Error("drain", "error", err)
		}
	}()

	sc := echo.StartConfig{
		Address: *addr,
		// Both example mains print their own one-line summary below; Echo's
		// own banner and "http(s) server started" log would otherwise
		// duplicate it.
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: timeout,
		OnShutdownError: func(err error) { slog.Error("shutdown", "error", err) },
		BeforeServeFunc: func(s *http.Server) error {
			s.ReadHeaderTimeout = timeout
			return nil
		},
	}

	slog.Info("serving GraphQL", "addr", *addr,
		"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
	err = sc.Start(ctx, e)
	// Start waits for its own graceful shutdown but not for the drain, which
	// may still be closing WebSockets; returning would exit the process under
	// them. Only a signal starts the drain, so a server that failed to start
	// returns at once.
	if ctx.Err() != nil {
		<-drained
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
