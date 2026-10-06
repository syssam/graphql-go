package transport_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlmux"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

const stallTimeout = 200 * time.Millisecond

// blobServers starts every handler that answers a query over SSE, each with
// the same short write timeout, in front of a query whose result is far more
// than a socket's buffers hold.
func blobServers(t *testing.T) map[string]string {
	t.Helper()
	blob := strings.Repeat("x", 32<<20)
	mk := func() *graphql.Executor {
		s, err := graphql.NewSchema(graphql.SDL(`type Query { blob: String! }`),
			graphql.Query(graphql.Field("blob", func(graphql.Root) string { return blob })))
		if err != nil {
			t.Fatalf("schema: %v", err)
		}
		return graphql.NewExecutor(s)
	}

	sse := httptest.NewServer(gqlsse.New(mk(), gqlsse.WithWriteTimeout(stallTimeout)))
	t.Cleanup(sse.Close)

	e := echo.New()
	e.Any("/", gqlecho.SSE(mk(), gqlsse.WithWriteTimeout(stallTimeout)))
	echoSrv := httptest.NewServer(e)
	t.Cleanup(echoSrv.Close)

	mux := httptest.NewServer(gqlmux.New(mk(), gqlmux.WithWriteTimeout(stallTimeout)))
	t.Cleanup(mux.Close)

	app := fiber.New()
	app.All("/", gqlfiber.SSE(mk(), gqlfiber.WithWriteTimeout(stallTimeout)))

	return map[string]string{
		"gqlsse":       sse.URL,
		"gqlecho.SSE":  echoSrv.URL,
		"gqlmux":       mux.URL,
		"gqlfiber.SSE": startFiberEquiv(t, app),
	}
}

var blobHandlers = []string{"gqlsse", "gqlecho.SSE", "gqlmux", "gqlfiber.SSE"}

// askForBlob sends the query over SSE on a raw connection and returns it once
// the status line has arrived, with nothing of the body read.
func askForBlob(t *testing.T, base string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	const body = `{"query":"{ blob }"}`
	// Connection: close, so that the end of the response is the end of the
	// connection and reading to EOF is reading the response.
	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Type: application/json\r\nAccept: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReaderSize(conn, 512)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("status line = %q, %v", status, err)
	}
	return conn, br
}

// tail keeps the end of what is written through it.
type tail struct{ b []byte }

func (w *tail) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	if len(w.b) > 64 {
		w.b = w.b[len(w.b)-64:]
	}
	return len(p), nil
}

// The write timeout is documented as what ends a client that stays connected
// and stops reading, and it was applied to a subscription's stream only. A
// query's result is one next and a complete on a stream of its own, written
// with no deadline: a client that asked for a large one and read nothing held
// the handler and the response buffer until it chose to leave, and
// http.Server.Shutdown waited for it.
//
// What a client can see of the fix is that the server gave up: after stalling
// for several timeouts it reads on and the stream ends short, without the
// complete an unbounded write would eventually have delivered.
func TestAStalledReaderOfAQueryOverSSEIsEndedByTheWriteTimeout(t *testing.T) {
	servers := blobServers(t)
	for _, name := range blobHandlers {
		t.Run(name, func(t *testing.T) {
			_, br := askForBlob(t, servers[name])
			time.Sleep(8 * stallTimeout)

			var end tail
			_, _ = io.Copy(&end, br)
			if strings.Contains(string(end.b), "event: complete") {
				t.Fatalf("the whole response arrived after a stall of %v under a write timeout of %v: no deadline bounded the write",
					8*stallTimeout, stallTimeout)
			}
		})
	}
}

// The other side of the same rule: the timeout is on a write that makes no
// progress, not on the response. A deadline over the whole result would bound
// its size by the client's bandwidth, so a reader that is slow and steady
// takes many timeouts over this one and still gets all of it.
func TestASlowReaderOfAQueryOverSSEIsNotCutOff(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 32 MiB slowly from four servers")
	}
	servers := blobServers(t)
	for _, name := range blobHandlers {
		t.Run(name, func(t *testing.T) {
			_, br := askForBlob(t, servers[name])

			var end tail
			start := time.Now()
			buf := make([]byte, 512<<10)
			for {
				n, err := io.ReadFull(br, buf)
				_, _ = end.Write(buf[:n])
				if err != nil {
					break
				}
				time.Sleep(15 * time.Millisecond)
			}
			took := time.Since(start)
			if !strings.Contains(string(end.b), "event: complete") {
				t.Fatalf("the response was cut off after %v; it ends %q", took, end.b)
			}
			if took < 2*stallTimeout {
				t.Skipf("read in %v, under two timeouts: too fast here to show a whole-response deadline", took)
			}
		})
	}
}
