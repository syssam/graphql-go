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
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// NewSchema takes whatever bindings the entity modules registered. It does not
// name them: a module that is not in the app leaves its types unbound, and
// graph.NewSchema says which.
func NewSchema(r resolve.Registered) (*graphql.Schema, error) {
	return graph.NewSchema(append(r.Bindings, scalars)...)
}

func NewExecutor(s *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(s)
}

// NewEcho registers every route with Any, for the reason examples/echo
// gives: each handler answers the methods it does not serve with its own
// GraphQL-aware error, which Echo's router must not pre-empt.
func NewEcho(exec *graphql.Executor, d *drain.Drain) *echo.Echo {
	e := echo.New()
	e.Any("/graphql", gqlecho.New(exec))
	e.Any("/graphql/stream", gqlecho.SSE(exec, gqlsse.WithDrain(d)))
	e.Any("/graphql/ws", gqlecho.WS(exec, gqlws.WithDrain(d)))
	return e
}

// timeout is ReadHeaderTimeout, and the bound on shutdown.
const timeout = 5 * time.Second

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
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			drained := make(chan error, 1)
			go func() { drained <- d.Shutdown(ctx) }()
			err := s.srv.Shutdown(ctx)
			return errors.Join(<-drained, err)
		},
	})
	return s
}
