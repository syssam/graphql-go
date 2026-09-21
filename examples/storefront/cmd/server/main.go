// Command server serves the storefront schema with the wiring a deployment
// needs and the other examples leave out: request limits, a cost budget per
// caller, persisted queries, OpenTelemetry, and a shutdown that winds down
// streams the HTTP server cannot.
//
//	/graphql         queries and mutations
//	/graphql/stream  Server-Sent Events
//	/graphql/ws      graphql-transport-ws
//
// Authentication is a header, so the example runs with curl:
//
//	curl -H 'authorization: staff'       ...  # every scope
//	curl -H 'authorization: support'     ...  # every order, no PII, no margin
//	curl -H 'authorization: customer:c1' ...  # only c1's own orders
//	curl                                 ...  # anonymous: Query.health only
//
// A real deployment verifies a token and puts its claims in the same place.
// Nothing below the transport knows the difference.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/storefront"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/ext/otel"
	"github.com/syssam/graphql-go/ext/throttle"
	"github.com/syssam/graphql-go/transport/drain"
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

// run holds every deferred cleanup, so main can exit non-zero without
// skipping any of it.
func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	schema, _, err := storefront.New()
	if err != nil {
		return fmt.Errorf("building schema: %w", err)
	}

	exec := graphql.NewExecutor(schema, executorOptions()...)

	// Runtime metrics are registered after the executor and the drain exist,
	// and each must be observed once per meter: two registrations of the same
	// executor add every value into one series and silently double it.
	unregister, err := otel.ObserveExecutor(exec)
	if err != nil {
		return fmt.Errorf("observing the executor: %w", err)
	}
	defer func() { _ = unregister.Unregister() }()

	// One cache shared by every transport, so a hash registered over HTTP is
	// usable from a WebSocket.
	persisted := apq.NewCache(512)

	// The drain winds down what srv.Shutdown cannot: an SSE stream is a
	// request that never ends, and a WebSocket is hijacked, which Shutdown
	// neither closes nor waits for.
	d := drain.New()
	unregisterDrain, err := otel.ObserveDrain(d)
	if err != nil {
		return fmt.Errorf("observing the drain: %w", err)
	}
	defer func() { _ = unregisterDrain.Unregister() }()

	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(exec,
		gqlhttp.WithPersistedQueries(persisted),
		gqlhttp.WithMaxBodyBytes(1<<20),
	))
	mux.Handle("/graphql/stream", gqlsse.New(exec,
		gqlsse.WithPersistedQueries(persisted),
		gqlsse.WithDrain(d),
		// An idle stream writes nothing, so without a keep-alive a dead
		// client is indistinguishable from a quiet one until its source ends.
		gqlsse.WithKeepAlive(15*time.Second),
	))
	mux.Handle("/graphql/ws", gqlws.New(exec,
		gqlws.WithPersistedQueries(persisted),
		gqlws.WithDrain(d),
		gqlws.WithInitTimeout(10*time.Second),
		gqlws.WithPingInterval(20*time.Second),
		// The WebSocket authenticates from connection_init rather than from a
		// header, because a browser cannot set one on a WebSocket. The
		// context it returns is the one every operation on the connection
		// runs under, so cancelling it -- on a token expiring, say -- closes
		// the socket.
		gqlws.WithOnConnect(func(ctx context.Context, payload []byte) (context.Context, error) {
			return storefront.WithPrincipal(ctx, principalFromInit(payload)), nil
		}),
	))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           authenticate(mux),
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
		// Together, not one after the other: srv.Shutdown waits for the SSE
		// handlers, and an SSE handler only returns once the drain ends it.
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

	slog.Info("serving GraphQL", "addr", *addr,
		"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		// Failed to start, so no shutdown began and there is nothing to wait
		// for.
		return err
	}
	// ListenAndServe returns when Shutdown begins, not when it ends.
	<-done
	return nil
}

// executorOptions is the whole executor configuration, in the order that
// matters.
//
// Ordering is load-bearing in one place: the engine's own cost check is the
// innermost layer of the operation chain, and an interceptor registered
// earlier sits further out. The throttle is an operation interceptor, so it
// runs before the engine would compute the cost -- which is why a cost model
// is configured at all, and why a limiter must be registered before any
// interceptor that should not run for a request it rejects.
func executorOptions() []graphql.ExecutorOption {
	opts := storefront.ExecutorOptions()

	opts = append(opts,
		// Structural limits, checked from the document before a plan is
		// built, so a refused query costs a walk of its AST and no more.
		graphql.WithMaxDepth(12),
		graphql.WithMaxComplexity(2000),

		// A cost budget with the numbers reported back, so a client can see
		// what it spent. Actual asks the engine to count what really
		// resolved alongside what was assumed, which is the only way to tell
		// whether DefaultListSize is set anywhere near reality.
		graphql.WithQueryCost(graphql.QueryCost{
			Max:             5000,
			DefaultListSize: 20,
			FieldWeight:     map[string]int{"Order.customer": 5},
			Report:          true,
			Actual:          true,
		}),

		// Bounds on one request's appetite. The timeout bounds work, not
		// latency: a resolver that ignores its context still holds the
		// response.
		graphql.WithOperationTimeout(15*time.Second),
		graphql.WithMaxResponseBytes(8<<20),
	)

	// Points per caller, refilled on read. It charges per subscription event
	// as well, because each event runs the whole operation chain -- a stream
	// billed once at open is unmetered.
	opts = append(opts, throttle.New(throttle.Config{
		MaximumAvailable: 20_000,
		RestoreRate:      1_000,
		Key: func(ctx context.Context) string {
			return storefront.PrincipalFrom(ctx).Subject
		},
	}))

	// One span per request, started before parsing so a parse failure still
	// produces one. Field spans are off: they are two interface calls and a
	// defer per field, including pure ones.
	opts = append(opts, otel.New()...)

	return opts
}

// authenticate turns the request's credentials into a Principal once, at the
// edge. Everything below reads it from the context, so no resolver and no
// policy parses a header.
func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFromHeader(r.Header.Get("Authorization"))
		next.ServeHTTP(w, r.WithContext(storefront.WithPrincipal(r.Context(), p)))
	})
}

// principalFromHeader is the example's stand-in for verifying a token. It
// recognises three roles and a customer, and anything it does not recognise
// is anonymous -- failing closed, because a credential the server cannot read
// is not a credential.
func principalFromHeader(h string) storefront.Principal {
	value := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	switch {
	case value == "staff":
		return storefront.Staff("staff")
	case value == "support":
		return storefront.Support("support")
	case strings.HasPrefix(value, "customer:"):
		return storefront.CustomerPrincipal(strings.TrimPrefix(value, "customer:"))
	default:
		return storefront.Anonymous()
	}
}

// principalFromInit reads the same credential out of a connection_init
// payload, which is where a browser puts it when it cannot set a header.
func principalFromInit(payload []byte) storefront.Principal {
	var init struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(payload, &init); err != nil {
		return storefront.Anonymous()
	}
	return principalFromHeader(init.Authorization)
}
