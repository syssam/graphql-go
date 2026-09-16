package gqlfiber

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

type ctxKey struct{}

// Fiber's own Ctx can never be cancelled, so the executor would have nothing
// to unwind an operation through. The derived context must cancel once the
// handler returns -- not on client disconnect, which Context() cannot see --
// while still carrying whatever an application put on the request context.
func TestRequestContextCancelsAndInheritsValues(t *testing.T) {
	var (
		got  context.Context
		done <-chan struct{}
	)
	app := fiber.New()
	app.Get("/graphql", func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(context.Background(), ctxKey{}, "set-by-middleware"))

		ctx, cancel := requestContext(c)
		defer cancel()
		got, done = ctx, ctx.Done()

		if done == nil {
			t.Error("Done() = nil, want a cancellable context")
		}
		select {
		case <-done:
			t.Error("context cancelled before the handler returned")
		default:
		}
		return nil
	})

	if _, err := app.Test(httptest.NewRequest(http.MethodGet, "/graphql", nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if v, _ := got.Value(ctxKey{}).(string); v != "set-by-middleware" {
		t.Errorf("Value = %q, want the value set with SetContext", v)
	}
	select {
	case <-done:
	default:
		t.Error("context still live after the handler returned; cancel did not run")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := newConfig()
	if cfg.maxBody != 1<<20 {
		t.Errorf("maxBody = %d, want 1 MiB", cfg.maxBody)
	}
	if cfg.batchMax != 0 {
		t.Errorf("batchMax = %d, want batching disabled", cfg.batchMax)
	}
	if !cfg.csrf {
		t.Error("csrf = false, want enabled")
	}
	if len(cfg.csrfHeaders) != len(httpreq.DefaultCSRFHeaders) {
		t.Errorf("csrfHeaders = %v, want %v", cfg.csrfHeaders, httpreq.DefaultCSRFHeaders)
	}
	if cfg.apq != nil {
		t.Error("apq is set, want disabled")
	}
	if cfg.keepAlive != 15*time.Second {
		t.Errorf("keepAlive = %v, want 15s", cfg.keepAlive)
	}
	if cfg.initTimeout != 10*time.Second {
		t.Errorf("initTimeout = %v, want 10s", cfg.initTimeout)
	}
	if cfg.pingInterval != 20*time.Second {
		t.Errorf("pingInterval = %v, want 20s", cfg.pingInterval)
	}
	if cfg.maxSubs != 100 {
		t.Errorf("maxSubs = %d, want 100", cfg.maxSubs)
	}
	if cfg.readLimit != 1<<20 {
		t.Errorf("readLimit = %d, want 1 MiB", cfg.readLimit)
	}
	if cfg.onConnect != nil {
		t.Error("onConnect is set, want nil")
	}
	if cfg.logger != slog.Default() {
		t.Error("logger is not slog.Default")
	}
}

func TestOptionsOverrideDefaults(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := newConfig(
		WithMaxBodyBytes(7),
		WithBatching(3),
		WithCSRFPrevention(false, "X-Only"),
		WithKeepAlive(time.Second),
		WithInitTimeout(2*time.Second),
		WithPingInterval(3*time.Second),
		WithMaxSubscriptions(4),
		WithReadLimit(5),
		WithOnConnect(func(ctx context.Context, _ []byte) (context.Context, error) { return ctx, nil }),
		WithLogger(logger),
	)
	if cfg.maxBody != 7 || cfg.batchMax != 3 || cfg.csrf || cfg.keepAlive != time.Second ||
		cfg.initTimeout != 2*time.Second || cfg.pingInterval != 3*time.Second ||
		cfg.maxSubs != 4 || cfg.readLimit != 5 || cfg.onConnect == nil || cfg.logger != logger {
		t.Errorf("config = %+v, want every option applied", cfg)
	}
	if len(cfg.csrfHeaders) != 1 || cfg.csrfHeaders[0] != "X-Only" {
		t.Errorf("csrfHeaders = %v, want [X-Only]", cfg.csrfHeaders)
	}
}
