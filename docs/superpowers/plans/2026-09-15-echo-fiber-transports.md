# Echo v5 and Fiber v3 Transports Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve GraphQL over Echo v5 and Fiber v3 — queries, mutations and subscriptions across HTTP, SSE and WebSocket — without duplicating the client-visible rules the existing transports already implement.

**Architecture:** Two shared internals are widened first, then two transports are built on them. `internal/httpreq` gains a `Source` accessor so fasthttp can reach the same CSRF, body-limit and decoding rules as `net/http`. The `graphql-transport-ws` state machine moves from `transport/gqlws/conn.go` into `internal/gqlwsproto` behind a `Socket` interface, so `coder/websocket` and `fasthttp/websocket` drive one implementation. Both moves are pure refactors that must leave existing tests untouched and green. `transport/gqlecho` is a thin `net/http` passthrough; `transport/gqlfiber` is fasthttp-native and writes responses straight into the fasthttp buffer.

**Tech Stack:** Go 1.27, `github.com/labstack/echo/v5 v5.3.1`, `github.com/gofiber/fiber/v3 v3.5.0`, `github.com/gofiber/contrib/v3/websocket v1.2.6`, `github.com/coder/websocket v1.8.15` (existing), `github.com/vektah/gqlparser/v2` (existing).

**Spec:** `docs/superpowers/specs/2026-09-15-echo-fiber-transports-design.md`

## Global Constraints

- **Branch:** `transport-echo-fiber`, cut from `phase4-otel`. Do not branch from `main`.
- **The gate for every task:** `go vet ./... && go test -race ./...` must pass before any commit. `-race` is not optional in this repository.
- **Root package may depend only on `gqlparser/v2` and the standard library.** Echo, Fiber and fasthttp may only be imported from `transport/gqlecho`, `transport/gqlfiber`, `examples/` and `benchmarks/`. `internal/httpreq` and `internal/gqlwsproto` import **standard library only** — no framework packages, no `coder/websocket`.
- **Module layout:** everything stays in the root module. Do not create new `go.mod` files except where this plan says so (`benchmarks/` already has one).
- **No reflection on the request hot path.** Reflection is permitted at `NewSchema` and in `Args[T]`/`Input[T]` decode only.
- **Go 1.27 minimum.**
- **Comments explain why, not what. English only. No code-narrating comments.**
- **Commit messages:** imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`). End every commit message with:
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`
- **Tests live beside the code**, in the package they test.
- **Two refactor tasks (1 and 2) must not modify any existing test file.** If an existing test needs editing to pass, the refactor changed behaviour: revert and rethink. This is the plan's single most important rule.

---

## File Structure

**Widened internals**

| File | Responsibility |
|---|---|
| `internal/httpreq/source.go` (create) | The `Source` accessor interface and the `net/http` adapter |
| `internal/httpreq/httpreq.go` (modify) | Existing rules, retargeted from `*http.Request` onto `Source` |
| `internal/gqlwsproto/protocol.go` (create) | Message types, close codes, subprotocol name — moved from `gqlws/protocol.go` |
| `internal/gqlwsproto/conn.go` (create) | The state machine — moved from `gqlws/conn.go` |
| `internal/gqlwsproto/config.go` (create) | `Config`, `ConnectFunc`, `Socket` |

**Existing transports, migrated**

| File | Responsibility |
|---|---|
| `transport/gqlhttp/handler.go` (modify) | Unchanged behaviour; calls `httpreq` through a `Source` |
| `transport/gqlsse/handler.go` (modify) | Same |
| `transport/gqlws/protocol.go` (modify) | Re-exports the protocol constants; package doc stays |
| `transport/gqlws/conn.go` (replace) | Shrinks to a `coder/websocket` `Socket` driver |
| `transport/gqlws/handler.go` (modify) | Builds a `gqlwsproto.Config`; public options unchanged |

**New transports**

| File | Responsibility |
|---|---|
| `transport/gqlecho/gqlecho.go` | Package doc, options, `net/http` → `echo.HandlerFunc` bridging |
| `transport/gqlecho/handler.go` | HTTP, SSE and WebSocket handler constructors |
| `transport/gqlfiber/gqlfiber.go` | Package doc, options, the cancellable-context helper |
| `transport/gqlfiber/source.go` | The fasthttp `httpreq.Source` adapter |
| `transport/gqlfiber/handler.go` | HTTP handler (queries, mutations, batching) |
| `transport/gqlfiber/sse.go` | SSE handler |
| `transport/gqlfiber/ws.go` | WebSocket `Socket` driver |

**Verification, examples, docs**

| File | Responsibility |
|---|---|
| `transport/equivalence_test.go` (package `transport_test`) | One table driving all four HTTP transports |
| `examples/echo/` | Self-contained Echo v5 example |
| `examples/fiber/` | Self-contained Fiber v3 example |
| `benchmarks/transport_bench_test.go` | The four-way performance matrix |
| `docs/benchmarks.md` (modify) | Results |
| `CLAUDE.md` (modify) | Transports section and dependency note |

---

## Task 1: Widen `internal/httpreq` to a `Source` accessor

Pure refactor. `gqlhttp` and `gqlsse` behaviour must not move, and **no existing test file may be edited**.

**Files:**
- Create: `internal/httpreq/source.go`
- Create: `internal/httpreq/source_test.go`
- Modify: `internal/httpreq/httpreq.go`
- Modify: `transport/gqlhttp/handler.go`
- Modify: `transport/gqlsse/handler.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Source interface { Method() string; Header(name string) string; QueryParam(name string) string; Body(max int64) ([]byte, error) }`
  - `func FromRequest(w http.ResponseWriter, r *http.Request) Source`
  - `var ErrBodyTooLarge = errors.New("request body too large")`
  - Retargeted: `func Forgeable(src Source, headers []string) bool`, `func ParseGET(src Source, queryOptional bool) (*graphql.Request, error)`, `func RequireJSONBody(src Source) error`, `func ReadBody(src Source, max int64) ([]byte, int, error)`
  - Unchanged: `Decode`, `IsJSONObject`, `MediaTypeJSON`, `DefaultCSRFHeaders`, `ErrMissingQuery`, `ErrMissingQueryParam`

- [ ] **Step 1: Write the failing test**

Create `internal/httpreq/source_test.go`:

```go
package httpreq

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromRequestReadsTheRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/graphql?query=%7Ba%7D", strings.NewReader(`{"query":"{a}"}`))
	r.Header.Set("Content-Type", "application/json")
	src := FromRequest(httptest.NewRecorder(), r)

	if got := src.Method(); got != http.MethodPost {
		t.Errorf("Method() = %q, want POST", got)
	}
	if got := src.Header("Content-Type"); got != "application/json" {
		t.Errorf("Header() = %q, want application/json", got)
	}
	if got := src.QueryParam("query"); got != "{a}" {
		t.Errorf("QueryParam() = %q, want {a}", got)
	}
	body, err := src.Body(1 << 20)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	if string(body) != `{"query":"{a}"}` {
		t.Errorf("Body() = %q", body)
	}
}

// An over-long body must be reportable as such, because ReadBody turns this
// into a 413 that every transport has to produce identically.
func TestFromRequestBodyTooLarge(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(strings.Repeat("x", 100)))
	src := FromRequest(httptest.NewRecorder(), r)

	if _, err := src.Body(10); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Body() error = %v, want ErrBodyTooLarge", err)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/httpreq/ -run TestFromRequest -v`
Expected: FAIL — `undefined: FromRequest`, `undefined: ErrBodyTooLarge`.

- [ ] **Step 3: Write `internal/httpreq/source.go`**

```go
package httpreq

import (
	"errors"
	"io"
	"net/http"
)

// ErrBodyTooLarge reports a body over the configured limit. Transports turn
// it into 413; the limit is enforced differently per transport but the
// client-visible result must not differ.
var ErrBodyTooLarge = errors.New("request body too large")

// Source is the request surface the transports share.
//
// It exists so that a transport built on something other than net/http --
// fasthttp, today -- reaches the same CSRF, body-limit and decoding rules
// rather than reimplementing them. A rule enforced here is enforced
// everywhere.
type Source interface {
	Method() string
	Header(name string) string
	QueryParam(name string) string
	Body(max int64) ([]byte, error)
}

// netHTTP adapts a net/http request. The ResponseWriter is held because
// MaxBytesReader needs it to mark the connection unusable after an
// over-long body.
type netHTTP struct {
	w http.ResponseWriter
	r *http.Request
}

// FromRequest adapts a net/http request to a Source.
func FromRequest(w http.ResponseWriter, r *http.Request) Source { return netHTTP{w: w, r: r} }

func (s netHTTP) Method() string                { return s.r.Method }
func (s netHTTP) Header(name string) string     { return s.r.Header.Get(name) }
func (s netHTTP) QueryParam(name string) string { return s.r.URL.Query().Get(name) }

func (s netHTTP) Body(max int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(s.w, s.r.Body, max))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/httpreq/ -run TestFromRequest -v`
Expected: PASS.

- [ ] **Step 5: Retarget the existing rules onto `Source`**

In `internal/httpreq/httpreq.go`, change these four signatures and their bodies. Everything else in the file stays exactly as it is, including all error strings — **the error text is part of the client contract and several tests assert it verbatim**.

```go
// Forgeable reports whether a browser could have sent the request
// cross-origin without a preflight: no Content-Type or one of the CORS
// "simple" types, and none of the preflight-forcing headers.
func Forgeable(src Source, headers []string) bool {
	for _, name := range headers {
		if src.Header(name) != "" {
			return false
		}
	}
	ct, _, err := mime.ParseMediaType(src.Header("Content-Type"))
	if err != nil {
		return true
	}
	switch ct {
	case "", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data":
		return true
	}
	return false
}

func ParseGET(src Source, queryOptional bool) (*graphql.Request, error) {
	req := &graphql.Request{Query: src.QueryParam("query"), OperationName: src.QueryParam("operationName")}
	if req.Query == "" && !queryOptional {
		return nil, ErrMissingQueryParam
	}
	if v := src.QueryParam("variables"); v != "" {
		if !IsJSONObject(v) {
			return nil, errors.New(`"variables" must be a JSON object`)
		}
		req.Variables = json.RawMessage(v)
	}
	if e := src.QueryParam("extensions"); e != "" {
		if err := json.Unmarshal([]byte(e), &req.Extensions); err != nil {
			return nil, fmt.Errorf(`"extensions" must be a JSON object: %v`, err)
		}
	}
	return req, nil
}

func RequireJSONBody(src Source) error {
	ct, _, err := mime.ParseMediaType(src.Header("Content-Type"))
	if err != nil || ct != MediaTypeJSON {
		return fmt.Errorf("Content-Type must be %s.", MediaTypeJSON)
	}
	return nil
}

// ReadBody reads at most max bytes. The returned status applies when err is
// non-nil, and distinguishes an over-long body from an unreadable one.
func ReadBody(src Source, max int64) ([]byte, int, error) {
	body, err := src.Body(max)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes.", max)
		}
		return nil, http.StatusBadRequest, fmt.Errorf("reading request body: %v", err)
	}
	return body, 0, nil
}
```

Remove the now-unused `io` import; keep `net/http` for the status constants.

- [ ] **Step 6: Migrate the two call sites**

In `transport/gqlhttp/handler.go`:
- `ServeHTTP` — build the source once after the method check: `src := httpreq.FromRequest(w, r)`.
- `httpreq.Forgeable(r, h.csrfHeaders)` → `httpreq.Forgeable(src, h.csrfHeaders)`.
- `httpreq.ParseGET(r, h.apq != nil)` → `httpreq.ParseGET(src, h.apq != nil)`.
- `parsePOST` — change the signature to `func (h *Handler) parsePOST(src httpreq.Source) (reqs []*graphql.Request, batch bool, status int, err error)`, and inside it `httpreq.RequireJSONBody(src)` and `httpreq.ReadBody(src, h.maxBody)`. Update the one call in `ServeHTTP` to `h.parsePOST(src)`.

In `transport/gqlsse/handler.go`, make the same substitutions in `ServeHTTP` and `parse`.

Do not change any error string, status code, or ordering of checks.

- [ ] **Step 7: Run the full gate**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS, with **zero edits to any existing `_test.go` file**. Confirm with:

Run: `git status --short "transport/**/*_test.go"`
Expected: no output. If any existing test file is modified, revert and rethink the refactor.

- [ ] **Step 8: Commit**

```bash
git add internal/httpreq transport/gqlhttp/handler.go transport/gqlsse/handler.go
git commit -m "refactor: widen httpreq to a Source accessor

A fasthttp transport cannot reach rules expressed over *http.Request, so
it would have to reimplement the CSRF check, the body limit and JSON
decoding -- the drift this package exists to prevent. Source is the
narrowest surface those rules need.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Extract `internal/gqlwsproto`

Pure refactor of a 387-line state machine. **`transport/gqlws/conn_test.go` must pass unmodified.**

Two hazards discovered while reading the existing code — both must be handled here:

1. **Write serialization.** `conn.write` currently relies on `coder/websocket` serializing writers internally. `fasthttp/websocket` is a gorilla derivative and does **not**: concurrent writes corrupt the stream. Since concurrent subscriptions all write to one socket, `gqlwsproto` takes a write mutex itself rather than trusting the driver. This costs `coder/websocket` an uncontended lock and makes the Fiber driver correct by construction.
2. **`RequestFrom` is `net/http`-specific.** `gqlws.RequestFrom(ctx) *http.Request` is existing public API and must not change. `gqlwsproto` therefore knows nothing about it: `Config.DecorateContext` lets a driver attach whatever it wants to the `ConnectFunc` context.

**Files:**
- Create: `internal/gqlwsproto/protocol.go`, `internal/gqlwsproto/config.go`, `internal/gqlwsproto/conn.go`
- Create: `internal/gqlwsproto/conn_test.go`
- Modify: `transport/gqlws/protocol.go`, `transport/gqlws/conn.go`, `transport/gqlws/handler.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `type Socket interface { Read(ctx context.Context) ([]byte, error); Write(ctx context.Context, data []byte) error; Close(code int, reason string) error }`
  - `type ConnectFunc func(ctx context.Context, initPayload []byte) (context.Context, error)`
  - `type Config struct { Exec *graphql.Executor; InitTimeout, PingInterval time.Duration; MaxSubs int; OnConnect ConnectFunc; DecorateContext func(context.Context) context.Context; Logger *slog.Logger }`
  - `func Serve(ctx context.Context, sock Socket, cfg Config)`
  - `const Subprotocol = "graphql-transport-ws"` and the seven `Status*` close codes.
  - `var ErrBinaryFrame = errors.New("gqlwsproto: binary frame")` — drivers return it from `Read` for a non-text frame.

- [ ] **Step 1: Write the failing test**

Create `internal/gqlwsproto/conn_test.go`. This is a new test of the extracted package through a fake socket; it does not replace `gqlws`'s tests.

```go
package gqlwsproto

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// fakeSocket is an in-memory Socket. It records every write so a test can
// assert the message sequence without a real connection.
type fakeSocket struct {
	in chan []byte

	mu     sync.Mutex
	out    [][]byte
	closed bool
	code   int
}

func newFakeSocket() *fakeSocket { return &fakeSocket{in: make(chan []byte, 16)} }

func (f *fakeSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return nil, context.Canceled
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeSocket) Write(ctx context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, append([]byte(nil), data...))
	return nil
}

func (f *fakeSocket) Close(code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed, f.code = true, code
		close(f.in)
	}
	return nil
}

func (f *fakeSocket) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.out))
	for _, b := range f.out {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(b, &m)
		out = append(out, m.Type)
	}
	return out
}

func (f *fakeSocket) closeCode() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code
}

func TestServeAcknowledgesInit(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: time.Second, MaxSubs: 10})
	}()

	// Closing the socket ends the read loop after the ack.
	time.Sleep(50 * time.Millisecond)
	_ = sock.Close(1000, "done")
	<-done

	got := sock.types()
	if len(got) == 0 || got[0] != "connection_ack" {
		t.Fatalf("message types = %v, want connection_ack first", got)
	}
}

// The init timeout must close the connection with 4408 rather than abort the
// read, or the client sees an abnormal closure and cannot tell why.
func TestServeInitTimeoutCloses4408(t *testing.T) {
	sock := newFakeSocket()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: 20 * time.Millisecond, MaxSubs: 10})
	}()
	<-done

	if got := sock.closeCode(); got != StatusInitTimeout {
		t.Fatalf("close code = %d, want %d", got, StatusInitTimeout)
	}
}

// A message before connection_init is unauthorized.
func TestServeSubscribeBeforeInitIsUnauthorized(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{a}"}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: time.Second, MaxSubs: 10})
	}()
	<-done

	if got := sock.closeCode(); got != StatusUnauthorized {
		t.Fatalf("close code = %d, want %d", got, StatusUnauthorized)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/gqlwsproto/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Move the protocol constants**

Create `internal/gqlwsproto/protocol.go` by copying the whole body of `transport/gqlws/protocol.go` below its package clause, changing `package gqlws` to `package gqlwsproto`, and **exporting the message-type constants** so a driver can name them: `TypeConnectionInit`, `TypeConnectionAck`, `TypePing`, `TypePong`, `TypeSubscribe`, `TypeNext`, `TypeError`, `TypeComplete`. The `Status*` close codes and `Subprotocol` keep their names. Rename `inMessage`/`outMessage`/`subscribePayload` to `InMessage`/`OutMessage`/`SubscribePayload`. Replace the package doc with one describing `gqlwsproto`, not `gqlws`.

Then rewrite `transport/gqlws/protocol.go` to keep the package doc (it documents the public `gqlws` package) and re-export the public names so existing users and tests are unaffected:

```go
package gqlws

import "github.com/syssam/graphql-go/internal/gqlwsproto"

// Subprotocol is the WebSocket subprotocol this handler negotiates. A client
// that does not offer it is closed with StatusSubprotocolNotAcceptable.
const Subprotocol = gqlwsproto.Subprotocol

// Close codes defined by the protocol, beyond the RFC 6455 range.
const (
	StatusSubprotocolNotAcceptable = gqlwsproto.StatusSubprotocolNotAcceptable
	StatusBadRequest               = gqlwsproto.StatusBadRequest
	StatusUnauthorized             = gqlwsproto.StatusUnauthorized
	StatusForbidden                = gqlwsproto.StatusForbidden
	StatusInitTimeout              = gqlwsproto.StatusInitTimeout
	StatusSubscriberExists         = gqlwsproto.StatusSubscriberExists
	StatusTooManyInitRequests      = gqlwsproto.StatusTooManyInitRequests
)
```

- [ ] **Step 4: Write `internal/gqlwsproto/config.go`**

```go
package gqlwsproto

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/syssam/graphql-go"
)

// ErrBinaryFrame is what a driver returns from Read for a frame that is not
// text. The protocol has no way to report a malformed frame against an
// operation, so it ends the connection.
var ErrBinaryFrame = errors.New("gqlwsproto: binary frame")

// Socket is the transport under the protocol. Implementations need not be
// safe for concurrent writes: Serve holds a lock across every Write.
type Socket interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close(code int, reason string) error
}

// ConnectFunc authenticates a connection from its connection_init payload.
// The returned context is the parent of every operation on that connection,
// so a token decoded here is available to every resolver.
type ConnectFunc func(ctx context.Context, initPayload []byte) (context.Context, error)

// Config is the protocol configuration a driver supplies.
type Config struct {
	Exec *graphql.Executor

	InitTimeout  time.Duration
	PingInterval time.Duration
	MaxSubs      int
	OnConnect    ConnectFunc

	// DecorateContext adds driver-specific values to the context a
	// ConnectFunc receives. gqlws uses it to carry the upgrade request,
	// which is where cookie auth arrives; a fasthttp driver carries its own
	// equivalent. Optional.
	DecorateContext func(context.Context) context.Context

	Logger *slog.Logger
}
```

- [ ] **Step 5: Write `internal/gqlwsproto/conn.go`**

Copy `transport/gqlws/conn.go` wholesale and apply these mechanical changes. Every comment in the original is preserved — they record why the code is shaped as it is.

1. `package gqlws` → `package gqlwsproto`. Drop the `coder/websocket` and `net/http` imports; add `sync` (already present) and `log/slog`.
2. `type conn struct` — replace `h *Handler` with `cfg Config`, and `ws *websocket.Conn` with `sock Socket`. Add `writeMu sync.Mutex`.
3. Every `c.h.X` becomes `c.cfg.X` (`exec`→`Exec`, `initTimeout`→`InitTimeout`, `pingInterval`→`PingInterval`, `maxSubs`→`MaxSubs`, `onConnect`→`OnConnect`, `logger`→`Logger`).
4. `serve(r *http.Request)` → `serve()`. In `handshake`, replace `withRequest(c.ctx, r)` with:

```go
hookCtx := c.ctx
if c.cfg.DecorateContext != nil {
	hookCtx = c.cfg.DecorateContext(c.ctx)
}
next, cerr := c.cfg.OnConnect(hookCtx, msg.Payload)
```

5. `read` loses its frame-type check (the driver owns framing now) and becomes:

```go
func (c *conn) read(ctx context.Context) (InMessage, error) {
	data, err := c.sock.Read(ctx)
	if err != nil {
		if errors.Is(err, ErrBinaryFrame) {
			c.close(StatusBadRequest, "Messages must be text frames")
		}
		return InMessage{}, err
	}
	var msg InMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.close(StatusBadRequest, "Invalid JSON message")
		return InMessage{}, err
	}
	if msg.Type == "" {
		c.close(StatusBadRequest, "Message is missing a type")
		return InMessage{}, errors.New("gqlwsproto: message without a type")
	}
	return msg, nil
}
```

6. `write` takes the lock. The original comment about `coder/websocket` serializing writers is now wrong and must be replaced:

```go
// write sends one message. The lock is the protocol's own: fasthttp/websocket
// corrupts the stream under concurrent writers, and concurrent operations all
// write here.
func (c *conn) write(ctx context.Context, msg OutMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.sock.Write(ctx, b)
}
```

7. `close` calls `c.sock.Close(code, reason)`, keeping the 123-byte reason cap and the debug log.
8. Add the entry point, replacing what `Handler.ServeHTTP` used to do after the upgrade:

```go
// Serve runs the protocol over sock until the connection ends. ctx is the
// connection's context, not any request's: the connection outlives the
// handshake.
func Serve(ctx context.Context, sock Socket, cfg Config) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &conn{
		cfg:    cfg,
		sock:   sock,
		ctx:    ctx,
		cancel: cancel,
		subs:   make(map[string]context.CancelFunc),
	}
	c.serve()
}
```

- [ ] **Step 6: Run the new test and verify it passes**

Run: `go test -race ./internal/gqlwsproto/ -v`
Expected: PASS — all three tests.

- [ ] **Step 7: Reduce `transport/gqlws` to a driver**

Replace `transport/gqlws/conn.go` entirely with a `coder/websocket` driver:

```go
package gqlws

import (
	"context"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go/internal/gqlwsproto"
)

// coderSocket drives the protocol over coder/websocket.
type coderSocket struct{ ws *websocket.Conn }

func (s coderSocket) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := s.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, gqlwsproto.ErrBinaryFrame
	}
	return data, nil
}

func (s coderSocket) Write(ctx context.Context, data []byte) error {
	return s.ws.Write(ctx, websocket.MessageText, data)
}

func (s coderSocket) Close(code int, reason string) error {
	return s.ws.Close(websocket.StatusCode(code), reason)
}
```

In `transport/gqlws/handler.go`, keep every exported option exactly as it is, and rewrite the tail of `ServeHTTP` (from the context comment onward) to:

```go
	// The connection outlives the HTTP request once the handshake is done, and
	// coder/websocket documents the request context as unsafe to use past
	// Accept, so the connection gets a context of its own.
	gqlwsproto.Serve(context.Background(), coderSocket{ws: ws}, gqlwsproto.Config{
		Exec:         h.exec,
		InitTimeout:  h.initTimeout,
		PingInterval: h.pingInterval,
		MaxSubs:      h.maxSubs,
		OnConnect:    gqlwsproto.ConnectFunc(h.onConnect),
		DecorateContext: func(ctx context.Context) context.Context {
			return withRequest(ctx, r)
		},
		Logger: h.logger,
	})
```

Keep `ConnectFunc` as a `gqlws` type alias so the public API is unchanged: `type ConnectFunc = gqlwsproto.ConnectFunc`. Leave `request.go` untouched — `RequestFrom` stays `net/http`-specific and public.

- [ ] **Step 8: Verify the existing WebSocket tests pass unmodified**

Run: `go test -race -count=1 ./transport/gqlws/ -v`
Expected: PASS.

Run: `git status --short transport/gqlws/conn_test.go`
Expected: no output. **If `conn_test.go` shows as modified, the refactor changed behaviour — revert this task and rethink.**

- [ ] **Step 9: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add internal/gqlwsproto transport/gqlws
git commit -m "refactor: extract graphql-transport-ws into internal/gqlwsproto

Fiber cannot use coder/websocket, so the protocol would otherwise be
written twice. The state machine moves behind a Socket interface;
handshake, origin checks and framing stay with each driver.

Serve now locks around every write. coder/websocket serializes writers
internally but fasthttp/websocket does not, and concurrent subscriptions
all write to one socket.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `transport/gqlecho` — HTTP

**Files:**
- Create: `transport/gqlecho/gqlecho.go`, `transport/gqlecho/handler.go`, `transport/gqlecho/handler_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `gqlhttp.New`, `gqlhttp.Option` (unchanged).
- Produces: `func New(exec *graphql.Executor, opts ...gqlhttp.Option) echo.HandlerFunc`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/labstack/echo/v5@v5.3.1`

- [ ] **Step 2: Write the failing test**

Create `transport/gqlecho/handler_test.go`:

```go
package gqlecho_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go/transport/gqlecho"
)

func TestPostQuery(t *testing.T) {
	e := echo.New()
	e.POST("/graphql", gqlecho.New(newTestExecutor(t)))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{hello}"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"data":{"hello":"world"}}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// A method the handler rejects must reach Echo as an HTTPError, so that an
// application's error handler and middleware observe it. This is the only
// behavioural reason to prefer this package over echo.WrapHandler.
func TestRejectedMethodIsAnEchoHTTPError(t *testing.T) {
	var seen error
	e := echo.New()
	e.HTTPErrorHandler = func(c *echo.Context, err error) { seen = err }
	e.Add(http.MethodDelete, "/graphql", gqlecho.New(newTestExecutor(t)))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/graphql", nil))

	he, ok := seen.(*echo.HTTPError)
	if !ok {
		t.Fatalf("error = %#v, want *echo.HTTPError", seen)
	}
	if he.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", he.Code)
	}
}
```

Create the shared fixture in `transport/gqlecho/fixture_test.go`. Build the smallest schema that answers `{hello}`:

```go
package gqlecho_test

import (
	"testing"

	"github.com/syssam/graphql-go"
)

func newTestExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	const sdl = `type Query { hello: String! }`
	s, err := graphql.NewSchema(sdl,
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}
```

Before writing this file, check `transport/gqlhttp/server_test.go` for the exact constructor spelling this repository uses and copy it — the binding API is `Query`/`Field`/`Resolve` but the precise generic form must match what compiles here.

- [ ] **Step 3: Run the test and verify it fails**

Run: `go test ./transport/gqlecho/ -v`
Expected: FAIL — package `gqlecho` does not exist.

- [ ] **Step 4: Write the implementation**

`transport/gqlecho/gqlecho.go`:

```go
// Package gqlecho serves GraphQL through Echo v5.
//
// Echo is built on net/http, so these handlers delegate to the transports in
// transport/gqlhttp, transport/gqlsse and transport/gqlws rather than
// reimplementing anything. What they add over echo.WrapHandler is that
// failures occurring before a GraphQL response exists -- a rejected method,
// an unacceptable Accept header, a forgeable request -- reach Echo as an
// *echo.HTTPError, so an application's error handler and middleware see them.
//
// GraphQL errors are not translated: a response carrying field errors is a
// successful HTTP response, and the status rules for
// application/graphql-response+json belong to gqlhttp.
package gqlecho

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// statusRecorder captures a transport-level status without buffering the
// body: only the status decides whether an HTTPError is raised, and the body
// of a successful response must stream straight through.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// serve runs h and reports any transport-level failure as an *echo.HTTPError.
func serve(c *echo.Context, h http.Handler) error {
	rec := &statusRecorder{ResponseWriter: c.Response(), status: http.StatusOK}
	h.ServeHTTP(rec, c.Request())

	// These four are the statuses the handlers emit before a GraphQL response
	// exists. Everything else, 400 included, is a GraphQL response envelope.
	switch rec.status {
	case http.StatusMethodNotAllowed, http.StatusNotAcceptable,
		http.StatusForbidden, http.StatusUnsupportedMediaType:
		return echo.NewHTTPError(rec.status, http.StatusText(rec.status))
	}
	return nil
}
```

`transport/gqlecho/handler.go`:

```go
package gqlecho

import (
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

// New returns a handler serving queries and mutations over HTTP. It accepts
// the same options as gqlhttp.New.
func New(exec *graphql.Executor, opts ...gqlhttp.Option) echo.HandlerFunc {
	h := gqlhttp.New(exec, opts...)
	return func(c *echo.Context) error { return serve(c, h) }
}
```

- [ ] **Step 5: Run the test and verify it passes**

Run: `go test -race ./transport/gqlecho/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum transport/gqlecho
git commit -m "feat: serve GraphQL over Echo v5

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: `transport/gqlecho` — SSE and WebSocket

**Files:**
- Modify: `transport/gqlecho/handler.go`
- Create: `transport/gqlecho/stream_test.go`

**Interfaces:**
- Consumes: `serve` from Task 3.
- Produces: `func SSE(exec *graphql.Executor, opts ...gqlsse.Option) echo.HandlerFunc`, `func WS(exec *graphql.Executor, opts ...gqlws.Option) echo.HandlerFunc`

- [ ] **Step 1: Write the failing test**

Create `transport/gqlecho/stream_test.go`. Model the assertions on `transport/gqlsse/handler_test.go` — read it first and reuse its event-parsing helper shape rather than inventing one.

```go
package gqlecho_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go/transport/gqlecho"
)

func TestSSEStreamsAQuery(t *testing.T) {
	e := echo.New()
	e.POST("/graphql", gqlecho.SSE(newTestExecutor(t)))

	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/graphql", strings.NewReader(`{"query":"{hello}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	var sawNext bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") && strings.Contains(sc.Text(), `"hello":"world"`) {
			sawNext = true
			break
		}
	}
	if !sawNext {
		t.Error("no data event carrying the result")
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./transport/gqlecho/ -run TestSSE -v`
Expected: FAIL — `undefined: gqlecho.SSE`.

- [ ] **Step 3: Add `SSE` and `WS`**

Append to `transport/gqlecho/handler.go`:

```go
// SSE returns a handler streaming results over Server-Sent Events. It accepts
// the same options as gqlsse.New.
func SSE(exec *graphql.Executor, opts ...gqlsse.Option) echo.HandlerFunc {
	h := gqlsse.New(exec, opts...)
	return func(c *echo.Context) error { return serve(c, h) }
}

// WS returns a handler speaking graphql-transport-ws. It accepts the same
// options as gqlws.New.
//
// The upgrade hijacks the connection, so no status reaches the recorder and
// no HTTPError is ever raised: a refused upgrade has already written its own
// response.
func WS(exec *graphql.Executor, opts ...gqlws.Option) echo.HandlerFunc {
	h := gqlws.New(exec, opts...)
	return func(c *echo.Context) error {
		h.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}
```

Add `gqlsse` and `gqlws` to the imports.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -race ./transport/gqlecho/ -v`
Expected: PASS.

- [ ] **Step 5: Add a WebSocket test**

Append to `stream_test.go` a test that dials the Echo-mounted `WS` handler with `coder/websocket`, sends `connection_init`, and asserts `connection_ack` comes back. Read `transport/gqlws/conn_test.go` first and reuse its dial helper rather than writing a new one.

- [ ] **Step 6: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add transport/gqlecho
git commit -m "feat: stream subscriptions over Echo v5

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: `transport/gqlfiber` — the fasthttp `Source` and the HTTP handler

**Files:**
- Create: `transport/gqlfiber/gqlfiber.go`, `transport/gqlfiber/source.go`, `transport/gqlfiber/handler.go`
- Create: `transport/gqlfiber/source_test.go`, `transport/gqlfiber/handler_test.go`, `transport/gqlfiber/fixture_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `httpreq.Source`, `httpreq.FromRequest` semantics, `httpreq.ErrBodyTooLarge` (Task 1).
- Produces:
  - `func New(exec *graphql.Executor, opts ...Option) fiber.Handler`
  - `type Option func(*config)`, with `WithMaxBodyBytes(int64)`, `WithBatching(int)`, `WithCSRFPrevention(bool, ...string)`, `WithPersistedQueries(apq.Cache)`, `WithLogger(*slog.Logger)` — matching `gqlhttp`'s option names exactly.
  - `func requestContext(c fiber.Ctx) (context.Context, context.CancelFunc)`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/gofiber/fiber/v3@v3.5.0`

- [ ] **Step 2: Write the failing `Source` test**

Create `transport/gqlfiber/source_test.go`:

```go
package gqlfiber

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// capture runs one request through a Fiber app and hands the handler's Source
// to fn.
func capture(t *testing.T, app *fiber.App, method, target, body string, fn func(httpreq.Source)) {
	t.Helper()
	app.Add([]string{method}, "/graphql", func(c fiber.Ctx) error {
		fn(newSource(c))
		return nil
	})
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestSourceReadsTheRequest(t *testing.T) {
	capture(t, fiber.New(), "POST", "/graphql?query=%7Ba%7D", `{"query":"{a}"}`, func(src httpreq.Source) {
		if got := src.Method(); got != "POST" {
			t.Errorf("Method() = %q, want POST", got)
		}
		if got := src.Header("Content-Type"); got != "application/json" {
			t.Errorf("Header() = %q", got)
		}
		if got := src.QueryParam("query"); got != "{a}" {
			t.Errorf("QueryParam() = %q, want {a}", got)
		}
		body, err := src.Body(1 << 20)
		if err != nil {
			t.Fatalf("Body() error = %v", err)
		}
		if string(body) != `{"query":"{a}"}` {
			t.Errorf("Body() = %q", body)
		}
	})
}

// Fiber reads bodies eagerly, so the limit is a length check rather than a
// capped reader. The error must still be the one ReadBody turns into a 413.
func TestSourceBodyTooLarge(t *testing.T) {
	capture(t, fiber.New(), "POST", "/graphql", strings.Repeat("x", 100), func(src httpreq.Source) {
		if _, err := src.Body(10); !errors.Is(err, httpreq.ErrBodyTooLarge) {
			t.Fatalf("Body() error = %v, want ErrBodyTooLarge", err)
		}
	})
	_ = io.Discard
}
```

- [ ] **Step 3: Run the test and verify it fails**

Run: `go test ./transport/gqlfiber/ -run TestSource -v`
Expected: FAIL — `undefined: newSource`.

- [ ] **Step 4: Write `transport/gqlfiber/source.go`**

```go
package gqlfiber

import (
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// source adapts a Fiber request to the shared rule set, so that the CSRF
// check, the body limit and JSON decoding are the ones gqlhttp and gqlsse
// already apply rather than a second implementation.
type source struct{ c fiber.Ctx }

func newSource(c fiber.Ctx) httpreq.Source { return source{c: c} }

func (s source) Method() string                { return s.c.Method() }
func (s source) Header(name string) string     { return s.c.Get(name) }
func (s source) QueryParam(name string) string { return s.c.Query(name) }

// Body enforces the limit as a length check: fasthttp has already read the
// whole body by the time a handler runs, so there is no reader left to cap.
// fiber.Config.BodyLimit is the defence that stops the read; this keeps the
// client-visible result identical to net/http's MaxBytesReader.
func (s source) Body(max int64) ([]byte, error) {
	b := s.c.Body()
	if int64(len(b)) > max {
		return nil, httpreq.ErrBodyTooLarge
	}
	return b, nil
}
```

- [ ] **Step 5: Run the test and verify it passes**

Run: `go test -race ./transport/gqlfiber/ -run TestSource -v`
Expected: PASS.

- [ ] **Step 6: Write the failing handler test**

Create `transport/gqlfiber/fixture_test.go` with the same `newTestExecutor` helper as Task 3 (package `gqlfiber`, not `gqlfiber_test`, since `source_test.go` is internal). Then `transport/gqlfiber/handler_test.go`:

```go
package gqlfiber

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestPostQuery(t *testing.T) {
	app := fiber.New()
	app.Post("/graphql", New(newTestExecutor(t)))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{hello}"}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got, want := strings.TrimSpace(string(body)), `{"data":{"hello":"world"}}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestOverLongBodyIs413(t *testing.T) {
	app := fiber.New()
	app.Post("/graphql", New(newTestExecutor(t), WithMaxBodyBytes(16)))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"`+strings.Repeat("x", 200)+`"}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}
```

- [ ] **Step 7: Run it and verify it fails**

Run: `go test ./transport/gqlfiber/ -run 'TestPostQuery|TestOverLong' -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 8: Write `gqlfiber.go` and `handler.go`**

`transport/gqlfiber/gqlfiber.go` holds the package doc, the `config` struct, the options, and the cancellation helper. The options mirror `gqlhttp`'s names and defaults exactly (`maxBody` 1 MiB, `csrf` true, `csrfHeaders` = `httpreq.DefaultCSRFHeaders`, batching off).

```go
// Package gqlfiber serves GraphQL through Fiber v3.
//
// Fiber is built on fasthttp, not net/http, so this is a native
// implementation rather than a wrapper: requests are parsed through the rules
// in internal/httpreq and responses are written straight into the fasthttp
// response buffer. Routing a request through middleware/adaptor would rebuild
// a synthetic *http.Request per call and cannot carry a WebSocket at all.
package gqlfiber

import (
	"context"

	"github.com/gofiber/fiber/v3"
)

// requestContext derives a cancellable context for one request.
//
// Fiber's Ctx documents itself as a context that can never be cancelled, and
// Context() is empty unless the application set one. The executor uses
// cancellation for teardown -- Subscribe's channel closes on it, and
// in-flight resolvers unwind through it -- so passing Fiber's own context
// would leave every subscription running after the client has gone.
func requestContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	return context.WithCancel(c.Context())
}
```

`transport/gqlfiber/handler.go` implements `New`. It mirrors `gqlhttp.Handler.ServeHTTP` step for step — same order of checks, same error strings, same statuses — with these substitutions:

- `negotiate(r.Header.Get("Accept"))` → `negotiate(c.Get("Accept"))`. Copy `negotiate` from `gqlhttp/handler.go` verbatim into this package; it is twenty lines of pure function and exporting it from `gqlhttp` would widen that package's API for no caller benefit.
- the CSRF, GET-parse and POST-parse calls take `newSource(c)`.
- `w.Header().Set("Allow", "GET, POST")` → `c.Set("Allow", "GET, POST")`.
- writing a response: `c.Set("Content-Type", mediaType+"; charset=utf-8")`, `c.Status(status)`, then `resp.WriteTo(c)` — `fiber.Ctx` is an `io.Writer`, so the JSON goes straight into the fasthttp buffer.
- `ctx, cancel := requestContext(c)` with `defer cancel()` replaces `r.Context()`.

- [ ] **Step 9: Run the tests and verify they pass**

Run: `go test -race ./transport/gqlfiber/ -v`
Expected: PASS.

- [ ] **Step 10: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add go.mod go.sum transport/gqlfiber
git commit -m "feat: serve GraphQL over Fiber v3

Fasthttp-native rather than a net/http bridge: requests parse through
internal/httpreq and responses write into the fasthttp buffer directly.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: `transport/gqlfiber` — SSE, and the cancellation guarantee

The cancellation test is the point of this task. Write it first and **watch it fail against a handler that passes `c.Context()` straight through**, or it proves nothing.

**Files:**
- Create: `transport/gqlfiber/sse.go`, `transport/gqlfiber/sse_test.go`

**Interfaces:**
- Consumes: `requestContext`, `newSource`, `config`, the options (Task 5).
- Produces: `func SSE(exec *graphql.Executor, opts ...Option) fiber.Handler`

- [ ] **Step 1: Write the failing cancellation test**

Create `transport/gqlfiber/sse_test.go`. The schema needs a subscription whose resolver reports when its context is cancelled; build it in the test rather than reusing `newTestExecutor`.

```go
package gqlfiber

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
)

// A dropped SSE client must cancel the operation context. Fiber's own context
// never cancels, so without the derived context in requestContext the
// subscription below runs forever.
func TestSSECancelsOnClientDisconnect(t *testing.T) {
	gone := make(chan struct{})

	const sdl = `type Query { hello: String! } type Subscription { ticks: Int! }`
	s, err := graphql.NewSchema(sdl,
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("ticks", func(ctx context.Context, _ graphql.Root) <-chan int {
			ch := make(chan int)
			go func() {
				defer close(ch)
				defer close(gone)
				t := time.NewTicker(5 * time.Millisecond)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						select {
						case ch <- 1:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			return ch
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}

	app := fiber.New()
	app.Post("/graphql", SSE(graphql.NewExecutor(s)))

	srv := startFiber(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv+"/graphql",
		strings.NewReader(`{"query":"subscription{ticks}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	buf := make([]byte, 128)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading first event: %v", err)
	}

	// Drop the client.
	cancel()
	resp.Body.Close()

	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription still running 3s after the client disconnected")
	}
}
```

Add `startFiber` to `fixture_test.go` — Fiber needs a real listener, because `app.Test` does not model a disconnect:

```go
// startFiber listens on a loopback port and returns the base URL.
func startFiber(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	return "http://" + ln.Addr().String()
}
```

Check `app.Listener`'s exact signature in Fiber v3 before using it (`go doc github.com/gofiber/fiber/v3.App.Listener`); v3 changed several `Listen` forms and may require a `fiber.ListenConfig`.

- [ ] **Step 2: Write a deliberately naive `sse.go` and watch the test fail**

Implement `SSE` passing `c.Context()` directly to the executor instead of `requestContext(c)`.

Run: `go test -race ./transport/gqlfiber/ -run TestSSECancels -v`
Expected: FAIL — "subscription still running 3s after the client disconnected". **If it passes, the test is not exercising the hazard; fix the test before continuing.**

- [ ] **Step 3: Fix `sse.go` to derive and cancel**

Structure `SSE` as:

1. Negotiate: require `text/event-stream` in `Accept`, else the same error `gqlsse` produces.
2. Parse through `newSource(c)` with the shared rules, exactly as Task 5's handler does.
3. `ctx, cancel := requestContext(c)`.
4. Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`.
5. `return c.SendStreamWriter(func(w *bufio.Writer) { ... })`, and inside:
   - `defer cancel()` — leaving the stream writer means the response is over.
   - run the operation, writing each event as `data: <json>\n\n` followed by `w.Flush()`.
   - **a `Flush` error is the disconnect signal**: call `cancel()` and return. It is the only one fasthttp offers.
   - drain the events channel after cancelling so the executor's pump is not left producing into a dead stream.
6. Honour `WithKeepAlive` by writing an SSE comment line (`:\n\n`) on a ticker, matching `gqlsse`'s keep-alive shape.

Match `gqlsse/handler.go`'s event framing byte for byte — read it and copy the `writeNext`/`writeComplete` output format rather than reconstructing it.

- [ ] **Step 4: Run the test and verify it now passes**

Run: `go test -race ./transport/gqlfiber/ -run TestSSECancels -v`
Expected: PASS.

- [ ] **Step 5: Add a plain streaming test**

Add `TestSSEStreamsAQuery` mirroring Task 4's Echo version, asserting the `Content-Type` and a `data:` line carrying `"hello":"world"`.

- [ ] **Step 6: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add transport/gqlfiber
git commit -m "feat: stream server-sent events over Fiber v3

A failed Flush is the only disconnect signal fasthttp offers, so it
drives the cancel. Fiber's own context never fires, and without this the
executor never learns the client left.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: `transport/gqlfiber` — WebSocket

**Files:**
- Create: `transport/gqlfiber/ws.go`, `transport/gqlfiber/ws_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `gqlwsproto.Serve`, `gqlwsproto.Config`, `gqlwsproto.Socket`, `gqlwsproto.ErrBinaryFrame` (Task 2).
- Produces: `func WS(exec *graphql.Executor, opts ...WSOption) fiber.Handler`, plus `WSOption` mirroring `gqlws`'s option names: `WithInitTimeout`, `WithPingInterval`, `WithMaxSubscriptions`, `WithReadLimit`, `WithOnConnect`, `WithLogger`.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/gofiber/contrib/v3/websocket@v1.2.6`

**Note the path.** `github.com/gofiber/contrib/websocket` is the Fiber **v2** module and will not build here; the v3 module is `github.com/gofiber/contrib/v3/websocket`.

- [ ] **Step 2: Write the failing test**

Create `transport/gqlfiber/ws_test.go`: start the app with `startFiber`, dial `ws://.../graphql` with `coder/websocket` offering the `graphql-transport-ws` subprotocol, send `{"type":"connection_init"}`, and assert the first message back has `"type":"connection_ack"`. Then send a `subscribe` for `{hello}` and assert a `next` followed by a `complete`. Read `transport/gqlws/conn_test.go` for the exact dial and assertion helpers and mirror them.

- [ ] **Step 3: Run it and verify it fails**

Run: `go test ./transport/gqlfiber/ -run TestWS -v`
Expected: FAIL — `undefined: WS`.

- [ ] **Step 4: Write `ws.go`**

```go
package gqlfiber

import (
	"context"

	"github.com/gofiber/contrib/v3/websocket"

	"github.com/syssam/graphql-go/internal/gqlwsproto"
)

// fastSocket drives the protocol over fasthttp/websocket.
//
// The library is a gorilla derivative: it has no per-message context and no
// internal write serialization. gqlwsproto locks around every write, so the
// methods below need no lock of their own; the context is unused because a
// read is unblocked by closing the connection, not by cancelling.
type fastSocket struct{ conn *websocket.Conn }

func (s fastSocket) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if typ != websocket.TextMessage {
		return nil, gqlwsproto.ErrBinaryFrame
	}
	return data, nil
}

func (s fastSocket) Write(ctx context.Context, data []byte) error {
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

func (s fastSocket) Close(code int, reason string) error {
	msg := websocket.FormatCloseMessage(code, reason)
	if err := s.conn.WriteMessage(websocket.CloseMessage, msg); err != nil {
		return err
	}
	return s.conn.Close()
}
```

`WS` then: rejects a non-upgrade request with 426 via `websocket.IsWebSocketUpgrade(c)`, calls `websocket.New(func(conn *websocket.Conn) { gqlwsproto.Serve(context.Background(), fastSocket{conn: conn}, cfg) })`, sets the read limit with `conn.SetReadLimit`, and negotiates the subprotocol through the `websocket.Config` field that carries it. Verify the exact spellings with `go doc github.com/gofiber/contrib/v3/websocket` before writing — this module is new and its API is not assumed here.

**Blocking-read caveat.** `ReadMessage` ignores the context, so a connection whose peer has vanished unblocks only when the socket errors. Set a read deadline from `PingInterval` if the library exposes one; if it does not, record the limitation in the package doc rather than leaving it unstated.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test -race ./transport/gqlfiber/ -v`
Expected: PASS.

- [ ] **Step 6: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add go.mod go.sum transport/gqlfiber
git commit -m "feat: speak graphql-transport-ws over Fiber v3

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Cross-transport equivalence

The rule set is shared; this proves it is, from the client's side.

**Files:**
- Create: `transport/equivalence_test.go` (package `transport_test`)

**Interfaces:**
- Consumes: `gqlhttp.New`, `gqlsse.New`, `gqlecho.New`, `gqlfiber.New`.
- Produces: nothing.

- [ ] **Step 1: Write the table test**

One table of cases, each driven through all four HTTP-carrying transports behind a `map[string]func(*graphql.Executor) http.Handler` — Fiber is adapted for the test only, via `httptest` against a real `startFiber` listener, since its handler is not an `http.Handler`.

Cases, each asserting **status and body identical across all four**:

| Case | Request | Expectation |
|---|---|---|
| over-long body | POST, body past `WithMaxBodyBytes(16)` | 413, `request body exceeds 16 bytes.` |
| forgeable request | POST, `Content-Type: text/plain`, CSRF on | 403 |
| non-JSON body | POST, `Content-Type: text/xml` | 415, `Content-Type must be application/json.` |
| mutation over GET | GET `?query=mutation{...}` | 405 |
| missing query, APQ off | POST `{}` | `request is missing the "query" member.` |
| missing query, APQ on | POST `{}` with `WithPersistedQueries` | unchanged from APQ off |

- [ ] **Step 2: Run it**

Run: `go test -race ./transport/ -run TestEquivalence -v`
Expected: PASS. Any divergence is a real defect in one of the new transports — fix the transport, never the expectation.

- [ ] **Step 3: Commit**

```bash
git add transport/equivalence_test.go
git commit -m "test: assert the four HTTP transports reject identically

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Examples

**Files:**
- Create: `examples/echo/` — `main.go`, `schema.graphql`, `resolvers.go`
- Create: `examples/fiber/` — `main.go`, `schema.graphql`, `resolvers.go`

Each is self-contained: its own SDL and resolvers, copyable as a starting point without tracing shared files. Model both on `examples/basic/` — read `examples/basic/main.go` and `examples/basic/schema/resolvers.go` first and follow their shape, including the broker pattern in `examples/basic/schema/broker.go` for the subscription.

Each `main.go` mounts three routes on one server: `POST /graphql` (queries and mutations), `POST /graphql/stream` (SSE), `GET /graphql/ws` (WebSocket), and prints the URLs on start.

- [ ] **Step 1: Build both**

Run: `go build ./examples/...`
Expected: success.

- [ ] **Step 2: Run each and check by hand**

Run: `go run ./examples/echo` then in another shell:
`curl -sS -H 'Content-Type: application/json' -H 'GraphQL-Require-Preflight: 1' -d '{"query":"{...}"}' localhost:8080/graphql`
Expected: a `data` payload. Repeat for `./examples/fiber`.

- [ ] **Step 3: Commit**

```bash
git add examples/echo examples/fiber
git commit -m "docs: add Echo v5 and Fiber v3 examples

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: The performance matrix

**Files:**
- Create: `benchmarks/transport_bench_test.go`
- Modify: `benchmarks/go.mod`, `benchmarks/go.sum`
- Modify: `docs/benchmarks.md`

**Interfaces:**
- Consumes: `gqlhttp.New`, `gqlecho.New`, `gqlfiber.New`, and `adaptor.HTTPHandler` from `github.com/gofiber/fiber/v3/middleware/adaptor`.
- Produces: nothing.

- [ ] **Step 1: Add the dependencies to the benchmarks module**

Run: `cd benchmarks && go get github.com/labstack/echo/v5@v5.3.1 github.com/gofiber/fiber/v3@v3.5.0`

- [ ] **Step 2: Write the four-way matrix**

Four rows — `NetHTTP`, `Echo`, `FiberNative`, `FiberAdaptor` — across two harnesses:

- `BenchmarkTransportInProcess/*` — invoke each handler directly, reporting `allocs/op` and `B/op`. This is where the `adaptor`'s synthetic `*http.Request` shows up.
- `BenchmarkTransportLoopback/*` — a real listener per framework, driven by one reused `http.Client` with keep-alive. Fasthttp's advantages are in connection and header handling, which the in-process harness does not touch at all.

Use the same query and the same schema for every row, and `b.ReportAllocs()` throughout. Reuse `benchmarks/gqlgo.go`'s schema construction rather than building a new one.

- [ ] **Step 3: Collect results properly**

Run:
```bash
cd benchmarks
go test -run '^$' -bench BenchmarkTransport -benchmem -count=10 > new.txt
benchstat new.txt
```
Ten counts and `benchstat` are not optional: single samples on this codebase have been wrong by 20-77% on a warm machine.

- [ ] **Step 4: Write the results up**

Add a section to `docs/benchmarks.md` with the `benchstat` table, the machine and Go version, and a plain reading of what it shows. **Report the Fiber-native vs Fiber-adaptor comparison honestly.** If native does not clearly win, say so — the row exists to answer that question, and a spec premise that did not hold is a finding worth recording.

- [ ] **Step 5: Commit**

```bash
git add benchmarks docs/benchmarks.md
git commit -m "test: benchmark the four HTTP transports

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: Documentation

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update the Transports section**

Extend it to name `gqlecho` and `gqlfiber`, and record the two facts a future reader would otherwise rediscover the hard way:

- Fiber's `Ctx` can never be cancelled and its `Context()` is empty, so every `gqlfiber` handler derives a cancellable context; without it subscriptions leak.
- `gqlwsproto` locks around every write because `fasthttp/websocket` does not serialize writers, unlike `coder/websocket`.

Also note that `internal/httpreq` now expresses its rules over a `Source`, so all four HTTP transports share them.

- [ ] **Step 2: Update the Conventions dependency note**

Add Echo, Fiber and `gofiber/contrib/v3/websocket` to the list of `go.mod` entries carried for sub-packages, and record that they split into their own module alongside `ext/otel` at publication time.

- [ ] **Step 3: Update the Status line**

Note Echo v5 and Fiber v3 transports as built.

- [ ] **Step 4: Run the full gate and commit**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

```bash
git add CLAUDE.md
git commit -m "docs: record the Echo and Fiber transports

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review Notes

**Spec coverage.** Spec 4.1 → Task 1. 4.2 → Task 2. 4.3 → Tasks 3, 4. 4.4 → Tasks 5, 6, 7. 4.5 → Global Constraints (one module). 5 → Task 9. 6.1/6.2 → Task 8. 6.3 → Task 2 Step 8. 6.4 → Task 6. 6.5 → every task's gate. 7 → Task 10. 9.7 → Task 11.

**Two hazards added during planning, not present in the spec:**
- `gqlwsproto` must serialize writes itself; `fasthttp/websocket` does not, and concurrent subscriptions share one socket. Task 2, Step 5.6.
- `gqlws.RequestFrom` is public and `net/http`-specific, so `Config.DecorateContext` keeps the protocol package framework-free without changing that API. Task 2, Step 4.

**Known soft spots, flagged rather than papered over:**
- Task 7 depends on a six-day-old module. Its API spellings are to be confirmed with `go doc` before writing, and the `ReadMessage` blocking-read caveat may prove to have no clean answer — record it in the package doc if so.
- Task 5 Step 8 describes `handler.go` structurally rather than in full, because it is a step-for-step mirror of `gqlhttp.Handler.ServeHTTP` and reproducing 150 lines here would invite drift from the file it must match. Read that file and follow it.
