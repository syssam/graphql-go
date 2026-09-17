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
	"sync"
	"syscall"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
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
	// The drain ends open subscription streams on shutdown; without it the
	// server's own Shutdown waits out its whole timeout for each one.
	d := drain.New()
	mux.Handle("/graphql/stream", gqlsse.New(exec, gqlsse.WithDrain(d)))

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
		// srv.Shutdown waits for every open SSE stream, and a stream only ends
		// once the drain ends it, so the two run together rather than one
		// after the other.
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := d.Shutdown(shutdownCtx); err != nil {
				slog.Error("drain", "error", err)
			}
		})
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
		wg.Wait()
	}()

	slog.Info("serving GraphQL", "addr", *addr, "http", "/graphql", "sse", "/graphql/stream")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		// Failed to start, so no shutdown began and there is nothing to wait
		// for.
		return err
	}
	// ListenAndServe returns as soon as Shutdown begins, not when it ends;
	// returning here would exit the process with streams still draining.
	<-done
	return nil
}
