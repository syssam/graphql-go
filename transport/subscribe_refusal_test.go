package transport_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// refusingExecutor refuses every subscription from an interceptor, with the
// kind of error a backend hands one, behind a presenter that shows a client
// nothing of any error.
func refusingExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL("type Query { hello: String! }\ntype Subscription { ticks: Int! }"),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) {
			return make(chan int), nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s,
		graphql.WithErrorPresenter(func(context.Context, error) *graphql.Error {
			return graphql.Errorf("masked")
		}),
		graphql.WithSubscriptionInterceptor(graphql.SubscriptionInterceptorFunc(
			func(context.Context, *graphql.OperationContext, graphql.SubscriptionHandler) (<-chan *graphql.Response, error) {
				return nil, errors.New("dial tcp 10.0.0.7:6379: connection refused")
			})))
}

// firstFrameAfterSubscribe opens a graphql-transport-ws connection, subscribes
// and returns the first message the server sends for it.
func firstFrameAfterSubscribe(t *testing.T, base string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http"),
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.CloseNow() }()
	for _, msg := range []string{
		`{"type":"connection_init"}`,
		`{"id":"1","type":"subscribe","payload":{"query":"subscription { ticks }"}}`,
	} {
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(data), `"id":"1"`) {
			return string(data)
		}
	}
}

// A subscription interceptor's own error is a refusal like any other and
// reaches a client the way the others do, through the ErrorPresenter. It was
// passed to the transport as it came, and all five streaming handlers sent
// its text -- a backend's address here -- past a presenter written to keep
// exactly that from a client.
func TestASubscriptionInterceptorsErrorReachesTheClientPresented(t *testing.T) {
	const masked = `{"errors":[{"message":"masked"}]}`

	t.Run("SSE", func(t *testing.T) {
		sse := httptest.NewServer(gqlsse.New(refusingExecutor(t)))
		t.Cleanup(sse.Close)
		e := echo.New()
		e.Any("/graphql", gqlecho.SSE(refusingExecutor(t)))
		echoSrv := httptest.NewServer(e)
		t.Cleanup(echoSrv.Close)
		app := fiber.New()
		app.All("/graphql", gqlfiber.SSE(refusingExecutor(t)))

		servers := map[string]string{"gqlsse": sse.URL, "gqlecho.SSE": echoSrv.URL, "gqlfiber.SSE": startFiberEquiv(t, app)}
		// 400, the status of every other request error on SSE and of an error
		// from the subscription's own opener.
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"}, `{"query":"subscription { ticks }"}`,
			http.StatusBadRequest, masked)
	})

	t.Run("WebSocket", func(t *testing.T) {
		ws := httptest.NewServer(gqlws.New(refusingExecutor(t)))
		t.Cleanup(ws.Close)
		app := fiber.New()
		app.Get("/", gqlfiber.WS(refusingExecutor(t)))

		const want = `{"id":"1","type":"error","payload":[{"message":"masked"}]}`
		for name, base := range map[string]string{"gqlws": ws.URL, "gqlfiber.WS": startFiberEquiv(t, app)} {
			if got := firstFrameAfterSubscribe(t, base); got != want {
				t.Errorf("%s sent %s, want %s", name, got, want)
			}
		}
	})
}
