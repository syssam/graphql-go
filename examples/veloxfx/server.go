package veloxfx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/graph"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
	"github.com/syssam/velox/contrib/graphqlgo"
)

// scalars binds Time, the one custom scalar gqlc does not: a models entry
// chooses time.Time, not its wire format. velox's Cursor encodes itself, and
// gqlc binds it.
var scalars = graphql.Time("Time")

// NewSchema takes whatever bindings the domain modules registered. It does
// not name them: a module that is not in the app leaves its types unbound,
// and graph.NewSchema says which.
func NewSchema(g gqlfx.Groups) (*graphql.Schema, error) {
	return graph.NewSchema(append(g.Bindings, scalars)...)
}

func NewExecutor(s *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(s, executorOptions()...)
}

// executorOptions is everything NewExecutor configures, apart so a benchmark
// can build a variant of the same executor.
func executorOptions() []graphql.ExecutorOption {
	return []graphql.ExecutorOption{
		graphql.WithErrorPresenter(presentError),
		graphqlgo.Collect(), // velox reads each query's selection
		graphql.WithAuthorizer(policy),
		// A public API is one bad query from a table scan. Each limit is
		// checked from the plan, before a resolver runs, so a refused query
		// costs no SQL at all.
		graphql.WithMaxDepth(10),
		graphql.WithQueryCost(graphql.QueryCost{
			// An ordinary browse page -- every category, twenty products
			// each, their stock -- is about 51,000; a thousand orders of every
			// customer with their items is millions.
			Max: 100_000,
			// A list with no first/last is assumed this long; velox pages
			// never exceed gqlrelay.MaxPaginationLimit (1000) either way.
			DefaultListSize: 50,
			// Every velox to-many edge is a Relay connection. Priced by the
			// page it asks for, products(first: 1) costs one row, not
			// DefaultListSize for its edges list: without this, fourteen
			// levels of first: 1 cost 260204, and legitimate pages are
			// refused or the budget is raised until it protects nothing.
			Connections: true,
		}),
		graphql.WithOperationTimeout(10 * time.Second),
	}
}

// NewEcho registers every route with Any, for the reason examples/echo
// gives: each handler answers the methods it does not serve with its own
// GraphQL-aware error, which Echo's router must not pre-empt.
func NewEcho(exec *graphql.Executor, d *drain.Drain) *echo.Echo {
	e := echo.New()
	e.Use(viewer.Middleware)
	e.Any("/graphql", gqlecho.New(exec))
	e.Any("/graphql/stream", gqlecho.SSE(exec, gqlsse.WithDrain(d)))
	e.Any("/graphql/ws", gqlecho.WS(exec, gqlws.WithDrain(d)))
	return e
}

// timeout is ReadHeaderTimeout.
const timeout = 5 * time.Second

// shutdownTimeout bounds OnStop. It must exceed five seconds: http.Server
// gives a connection that was opened but has sent nothing that long before
// Shutdown counts it idle, in case a request is on its way, and clients --
// load balancers, browsers, Go's own Transport under concurrency -- open
// such connections ahead of need. At five, one of them failed every stop.
const shutdownTimeout = 10 * time.Second

// Server is the listening half, kept apart from NewEcho so that a test can
// take the routes without a socket.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Addr is the address the server listens on, valid once the app has started.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// NewServer listens in OnStart, so a port already taken fails fx's start and
// the process exits non-zero, instead of failing later in a goroutine nobody
// waits on.
func NewServer(lc fx.Lifecycle, sd fx.Shutdowner, cfg Config, e *echo.Echo, d *drain.Drain) *Server {
	s := &Server{srv: &http.Server{Handler: e, ReadHeaderTimeout: timeout}}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			var listener net.ListenConfig
			ln, err := listener.Listen(ctx, "tcp", cfg.Addr)
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			s.ln = ln
			go func() {
				if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("serve", "error", err)
					_ = sd.Shutdown(fx.ExitCode(1))
				}
			}()
			slog.Info("serving GraphQL", "addr", s.Addr(),
				"http", "/graphql", "sse", "/graphql/stream", "ws", "/graphql/ws")
			return nil
		},
		// The drain runs first and alongside: http.Server.Shutdown waits for
		// every handler to return, and an SSE stream or a WebSocket returns
		// only when the drain ends it.
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
			defer cancel()
			drained := make(chan error, 1)
			go func() { drained <- d.Shutdown(ctx) }()
			err := s.srv.Shutdown(ctx)
			if err != nil {
				// Out of time: close what is left rather than leave it open
				// under a process that is about to exit, and still report it.
				err = errors.Join(err, s.srv.Close())
			}
			return errors.Join(<-drained, err)
		},
	})
	return s
}
