package gqlfiber

import (
	"net"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
)

// startFiber serves app on a loopback port and returns its base URL.
//
// A real listener is needed rather than app.Test: the latter drives the
// handler over an in-memory connection and so cannot model a client that
// goes away while a stream is open.
func startFiber(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		// Bounded, because a stream that failed to notice its client left
		// would otherwise keep the connection busy and hang the run instead
		// of letting the test that cares report the failure.
		_ = app.ShutdownWithTimeout(2 * time.Second)
	})
	return "http://" + ln.Addr().String()
}

func newTestExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Mutation { bump: String! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Mutation(graphql.Field("bump", func(graphql.Root) string { return "bumped" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}
