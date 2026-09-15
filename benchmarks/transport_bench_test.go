// This file measures what each HTTP transport costs on top of the same
// executor, and exists mainly to test one claim: that gqlfiber's
// fasthttp-native implementation earns its complexity over wrapping
// gqlhttp in fiber's middleware/adaptor. The FiberAdaptor row is there to
// falsify that claim, not to confirm it.
//
// Two harnesses, because they measure different things. The in-process one
// invokes each handler against a reused response object, so allocs/op and
// B/op belong to the transport rather than to a socket; it is where the
// adaptor's synthetic *http.Request has to show up if it costs anything.
// The loopback one drives real listeners through one shared http.Client, so
// connection reuse and header parsing -- fasthttp's actual territory, which
// the in-process harness never touches -- are in the number.
//
// Both run at two payload sizes. Per-request transport overhead is a fixed
// cost: a 100-user response hides it and a near-empty one isolates it, and
// only the pair says whether the overhead is worth anything in practice.
package benchmarks

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/labstack/echo/v5"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"github.com/syssam/graphql-go/benchmarks/internal/data"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const transportPath = "/graphql"

var transportQueries = []struct{ name, query string }{
	{"Tiny", `{ __typename }`},
	{"List", queryShallow},
}

var transportRows = []struct {
	name    string
	handler func() http.Handler
	app     func() *fiber.App
}{
	{name: "NetHTTP", handler: newNetHTTPHandler},
	{name: "Echo", handler: newEchoHandler},
	{name: "FiberNative", app: newFiberNativeApp},
	{name: "FiberAdaptor", app: newFiberAdaptorApp},
}

func transportBody(query string) []byte {
	b, err := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	if err != nil {
		panic(err)
	}
	return b
}

func newNetHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(transportPath, gqlhttp.New(newGraphQLGo(data.Dataset())))
	return mux
}

func newEchoHandler() http.Handler {
	e := echo.New()
	e.Any(transportPath, gqlecho.New(newGraphQLGo(data.Dataset())))
	return e
}

// All rather than Post so the GraphQL handler, not the router, decides every
// method -- the convention transport/equivalence_test.go and both examples
// use. The adaptor row is registered the same way so the two Fiber rows
// differ only in what sits behind the route.
func newFiberNativeApp() *fiber.App {
	app := fiber.New()
	app.All(transportPath, gqlfiber.New(newGraphQLGo(data.Dataset())))
	return app
}

func newFiberAdaptorApp() *fiber.App {
	app := fiber.New()
	app.All(transportPath, adaptor.HTTPHandler(gqlhttp.New(newGraphQLGo(data.Dataset()))))
	return app
}

// driver runs one request against one transport and returns the response
// body. The slice aliases the driver's own buffer, so a caller must not hold
// it across calls; nothing is copied, and the harness contributes no
// allocations of its own to the numbers.
type driver interface{ do() []byte }

// nullWriter is a net/http response sink that keeps its header map and its
// body buffer between requests, so that the net/http rows write into a
// reused buffer exactly as the Fiber rows write into a reused fasthttp
// response. httptest.NewRecorder would allocate a recorder and a buffer per
// iteration and charge it to the transport.
type nullWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *nullWriter) Header() http.Header         { return w.header }
func (w *nullWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *nullWriter) WriteHeader(int)             {}

func (w *nullWriter) reset() {
	clear(w.header)
	w.body.Reset()
}

// replayBody lets one *http.Request be sent repeatedly: the handler closes
// the body, and the driver rewinds the reader instead of allocating a new
// one.
type replayBody struct{ r *bytes.Reader }

func (b replayBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b replayBody) Close() error               { return nil }

type netHTTPDriver struct {
	h   http.Handler
	req *http.Request
	src *bytes.Reader
	w   *nullWriter
}

func newNetHTTPDriver(h http.Handler, body []byte) *netHTTPDriver {
	src := bytes.NewReader(body)
	req := httptest.NewRequest(http.MethodPost, transportPath, nil)
	req.Body = replayBody{r: src}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	return &netHTTPDriver{
		h:   h,
		req: req,
		src: src,
		w:   &nullWriter{header: make(http.Header)},
	}
}

func (d *netHTTPDriver) do() []byte {
	if _, err := d.src.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}
	d.w.reset()
	d.h.ServeHTTP(d.w, d.req)
	return d.w.body.Bytes()
}

// fakeConn stands in for the socket a fasthttp server would have attached.
// A zero RequestCtx has no server behind it, and the adaptor row reaches for
// one: it passes the RequestCtx itself as the request's context.Context, so
// the executor's ctx.Err() dereferences a nil server and panics.
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

// discardLogger keeps a transport that logs a warning from charging the
// benchmark for stderr.
type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

type fasthttpDriver struct {
	h   fasthttp.RequestHandler
	ctx *fasthttp.RequestCtx
}

func newFasthttpDriver(app *fiber.App, body []byte) *fasthttpDriver {
	ctx := &fasthttp.RequestCtx{}
	// reduceMemoryUsage false keeps the response body buffer across requests,
	// which is both fiber.New's own default and the counterpart of the
	// net/http rows' reused bytes.Buffer.
	ctx.Init2(fakeConn{}, discardLogger{}, false)
	ctx.Request.Header.SetMethod(http.MethodPost)
	ctx.Request.SetRequestURI(transportPath)
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBody(body)
	return &fasthttpDriver{h: app.Handler(), ctx: ctx}
}

func (d *fasthttpDriver) do() []byte {
	d.ctx.Response.Reset()
	d.h(d.ctx)
	return d.ctx.Response.Body()
}

func newDriver(row int, body []byte) driver {
	r := transportRows[row]
	if r.handler != nil {
		return newNetHTTPDriver(r.handler(), body)
	}
	return newFasthttpDriver(r.app(), body)
}

// TestTransportRowsAgree is the precondition for reading the benchmark at
// all: a row that answered with an error envelope, or skipped work the
// others do, would still produce a number.
func TestTransportRowsAgree(t *testing.T) {
	for _, q := range transportQueries {
		body := transportBody(q.query)
		var want []byte
		for i, row := range transportRows {
			got := bytes.Clone(newDriver(i, body).do())
			if i == 0 {
				want = got
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s/%s: response differs from %s\n got: %s\nwant: %s",
					q.name, row.name, transportRows[0].name, got, want)
			}
		}
	}
}

func BenchmarkTransportInProcess(b *testing.B) {
	for _, q := range transportQueries {
		body := transportBody(q.query)
		for i, row := range transportRows {
			b.Run(q.name+"/"+row.name, func(b *testing.B) {
				d := newDriver(i, body)
				want := len(d.do())
				b.ReportAllocs()
				for b.Loop() {
					if n := len(d.do()); n != want {
						b.Fatalf("response of %d bytes, want %d", n, want)
					}
				}
			})
		}
	}
}

// BenchmarkTransportAdaptorOverhead attributes the FiberAdaptor row's extra
// cost to the two things fasthttpadaptor does that the native handler does
// not, so the matrix can be checked against a mechanism instead of asserting
// one: it rebuilds an *http.Request per call, and it runs the net/http
// handler on a fresh goroutine, blocking on a channel until that goroutine
// reports back -- which is how it can react to a Flush or a Hijack.
func BenchmarkTransportAdaptorOverhead(b *testing.B) {
	b.Run("ConvertRequest", func(b *testing.B) {
		d := newFasthttpDriver(newFiberNativeApp(), transportBody(transportQueries[0].query))
		b.ReportAllocs()
		for b.Loop() {
			var r http.Request
			if err := fasthttpadaptor.ConvertRequest(d.ctx, &r, true); err != nil {
				b.Fatalf("ConvertRequest: %v", err)
			}
		}
	})

	b.Run("GoroutineHandoff", func(b *testing.B) {
		ch := make(chan struct{})
		b.ReportAllocs()
		for b.Loop() {
			go func() { ch <- struct{}{} }()
			<-ch
		}
	})

	// The bare handoff above is the cheap case: a goroutine that does nothing
	// parks its parent for almost no time and never outgrows its initial
	// stack. This row moves the NetHTTP row's own work onto a fresh goroutine
	// per request and changes nothing else, so the difference between it and
	// TransportInProcess/Tiny/NetHTTP is what the adaptor's concurrency costs
	// on this workload rather than on an empty one.
	//
	// Read that difference as an upper bound, for two reasons. The channel
	// here is unbuffered, so the child parks until the parent receives, where
	// the adaptor's modeCh is make(chan int, 1) written under a select/default
	// and never blocks. And it is a subtraction between two independently
	// noisy benchmarks whose subtrahend is the least stable row in the matrix:
	// across the two runs in docs/benchmarks.md it came out at +2.2µs and at
	// +0.8µs, which is why that document reports a range and declines to say
	// whether this or ConvertRequest is the larger cost.
	b.Run("HandlerOnGoroutine", func(b *testing.B) {
		d := newNetHTTPDriver(newNetHTTPHandler(), transportBody(transportQueries[0].query))
		ch := make(chan struct{})
		b.ReportAllocs()
		for b.Loop() {
			go func() {
				d.do()
				ch <- struct{}{}
			}()
			<-ch
		}
	})
}

// transportClient is shared by every loopback row so that no row gets a
// different connection pool, timeout or compression setting than another.
// One idle connection per host plus a serial loop means every row is
// measured over a single kept-alive connection.
var transportClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		DisableCompression:  true,
	},
	Timeout: 30 * time.Second,
}

func startNetHTTP(b *testing.B, h http.Handler) string {
	srv := httptest.NewServer(h)
	b.Cleanup(srv.Close)
	return srv.URL
}

// startFiber serves app on a real loopback port rather than through
// app.Test, whose in-memory fake connection would put the Fiber rows on a
// different code path than the net/http rows' real sockets.
func startFiber(b *testing.B, app *fiber.App) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	b.Cleanup(func() { _ = app.ShutdownWithTimeout(5 * time.Second) })
	return "http://" + ln.Addr().String()
}

func BenchmarkTransportLoopback(b *testing.B) {
	for _, q := range transportQueries {
		body := transportBody(q.query)
		for _, row := range transportRows {
			b.Run(q.name+"/"+row.name, func(b *testing.B) {
				var url string
				if row.handler != nil {
					url = startNetHTTP(b, row.handler())
				} else {
					url = startFiber(b, row.app())
				}
				post := func() int64 {
					req, err := http.NewRequest(http.MethodPost, url+transportPath, bytes.NewReader(body))
					if err != nil {
						b.Fatalf("NewRequest: %v", err)
					}
					req.Header.Set("Content-Type", "application/json")
					resp, err := transportClient.Do(req)
					if err != nil {
						b.Fatalf("Do: %v", err)
					}
					n, err := io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if err != nil {
						b.Fatalf("reading body: %v", err)
					}
					if resp.StatusCode != http.StatusOK {
						b.Fatalf("status %d", resp.StatusCode)
					}
					return n
				}
				want := post()
				b.ReportAllocs()
				for b.Loop() {
					if n := post(); n != want {
						b.Fatalf("response of %d bytes, want %d", n, want)
					}
				}
			})
		}
	}
}
