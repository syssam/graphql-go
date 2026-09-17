package gqlwsproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
)

// Serve runs the protocol over sock until the connection ends. ctx is the
// connection's context, not any request's: the connection outlives the
// handshake.
func Serve(ctx context.Context, sock Socket, cfg Config) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	streams, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()

	c := &conn{
		cfg:         cfg,
		sock:        sock,
		ctx:         ctx,
		cancel:      cancel,
		subs:        make(map[string]context.CancelFunc),
		streams:     streams,
		stopStreams: stopStreams,
	}
	served, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		c.watch(parent, served)
	}()
	c.serve()
	close(served)
	// The watcher may still owe the socket its 1001 close; returning first
	// would let the driver's own teardown race it. No test catches this
	// deterministically (removing the wait failed 3 of 50 runs), so keep it
	// on this reasoning rather than on a green suite.
	<-watched
}

// watch ends the connection from outside the read loop, which is the only way
// to reach a Read that ignores its context. It watches parent rather than the
// connection's own context, which Serve cancels on its way out: only a cancel
// from the caller -- a drain giving up -- should close the socket.
func (c *conn) watch(parent context.Context, served <-chan struct{}) {
	select {
	case <-c.cfg.Closing:
		c.drain(parent, served)
	case <-parent.Done():
		c.close(StatusGoingAway, "Going away")
	case <-served:
		// A cancelled parent also ends the read loop, so served can win the
		// race against parent.Done; the socket must still be closed then.
		if parent.Err() != nil {
			c.close(StatusGoingAway, "Going away")
		}
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// drain refuses new operations, ends subscriptions, waits for the rest, and
// closes. The close is what ends the read loop.
//
// A drain that gives up cancels parent, and by then this goroutine is parked
// here rather than in watch's select, so the wait must watch parent too: an
// operation that ignores cancellation would otherwise hold the socket open
// for as long as it runs.
func (c *conn) drain(parent context.Context, served <-chan struct{}) {
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()
	c.stopStreams()
	waited := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-parent.Done():
		c.close(StatusGoingAway, "Going away")
		return
	}
	select {
	case <-served:
		return // the connection ended on its own meanwhile
	default:
	}
	c.close(StatusGoingAway, "Going away")
}

// conn is one WebSocket connection. Operations run on their own goroutines so
// that the read loop stays responsive: a client must be able to send complete
// to cancel a subscription that is producing continuously.
type conn struct {
	cfg    Config
	sock   Socket
	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex

	mu   sync.Mutex
	subs map[string]context.CancelFunc

	// draining is set under mu, beside every wg.Add, so an operation either
	// starts before the drain waits or is refused.
	draining bool

	// streams parents every subscription and is cancelled by the drain;
	// queries and mutations do not derive from it, so they run to the end.
	streams     context.Context
	stopStreams context.CancelFunc

	wg sync.WaitGroup
}

func (c *conn) serve() {
	if !c.handshake() {
		return
	}
	defer func() {
		c.cancelAll()
		c.wg.Wait()
	}()

	stopPing := c.startPings()
	defer stopPing()

	for {
		msg, err := c.read(c.ctx)
		if err != nil {
			// A read failure is the end of the connection: either the client
			// closed it or the frame was unusable, and close already ran.
			return
		}
		if !c.dispatch(msg) {
			return
		}
	}
}

// handshake runs the connection_init exchange. It reports whether the
// connection is established; on failure it has already closed.
func (c *conn) handshake() bool {
	// The timeout closes the connection rather than bounding the read: a read
	// aborted by its own context leaves coder/websocket no way to send a close
	// frame, so the client would see an abnormal closure instead of 4408.
	timer := time.AfterFunc(c.cfg.InitTimeout, func() {
		c.close(StatusInitTimeout, "Connection initialisation timeout")
	})
	defer timer.Stop()

	ctx := c.ctx
	for {
		msg, err := c.read(ctx)
		if err != nil {
			return false
		}
		switch msg.Type {
		case TypeConnectionInit:
			opCtx := c.ctx
			if c.cfg.OnConnect != nil {
				hookCtx := c.ctx
				if c.cfg.DecorateContext != nil {
					hookCtx = c.cfg.DecorateContext(c.ctx)
				}
				next, cerr := c.cfg.OnConnect(hookCtx, msg.Payload)
				if cerr != nil {
					c.close(StatusForbidden, cerr.Error())
					return false
				}
				if next != nil {
					opCtx = next
				}
			}
			// The hook's context parents every operation, so a token decoded
			// there reaches every resolver on this connection.
			c.ctx, c.cancel = context.WithCancel(opCtx)
			if err := c.write(c.ctx, OutMessage{Type: TypeConnectionAck}); err != nil {
				return false
			}
			return true
		case TypePing:
			if err := c.write(ctx, OutMessage{Type: TypePong, Payload: msg.Payload}); err != nil {
				return false
			}
		case TypePong:
			// Unsolicited pongs are allowed and carry no state.
		default:
			c.close(StatusUnauthorized, "Unauthorized")
			return false
		}
	}
}

// dispatch handles one post-handshake message, reporting whether the
// connection should continue.
func (c *conn) dispatch(msg InMessage) bool {
	switch msg.Type {
	case TypePing:
		return c.write(c.ctx, OutMessage{Type: TypePong, Payload: msg.Payload}) == nil
	case TypePong:
		return true
	case TypeConnectionInit:
		c.close(StatusTooManyInitRequests, "Too many initialisation requests")
		return false
	case TypeSubscribe:
		return c.subscribe(msg)
	case TypeComplete:
		// The client is unsubscribing. The protocol has no acknowledgement:
		// a complete travels in each direction independently.
		c.stop(msg.ID)
		return true
	default:
		c.close(StatusBadRequest, fmt.Sprintf("Invalid message type %q", msg.Type))
		return false
	}
}

func (c *conn) subscribe(msg InMessage) bool {
	if msg.ID == "" {
		c.close(StatusBadRequest, "Subscribe message is missing an id")
		return false
	}

	var payload SubscribePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		c.close(StatusBadRequest, "Invalid subscribe payload")
		return false
	}
	req := &graphql.Request{
		Query:         payload.Query,
		OperationName: payload.OperationName,
		Variables:     payload.Variables,
		Extensions:    payload.Extensions,
	}

	ctx, cancel := context.WithCancel(c.ctx)

	c.mu.Lock()
	// The watcher sets draining asynchronously, so a subscribe read just after
	// Closing closed would otherwise be accepted and then silently cancelled.
	// That window is too narrow to test deterministically (removing the
	// channel check failed 0 of 50 runs); keep it on this reasoning.
	if c.draining || closed(c.cfg.Closing) {
		c.mu.Unlock()
		cancel()
		return c.writeError(msg.ID, graphql.Errorf("The server is shutting down.")) == nil
	}
	if _, exists := c.subs[msg.ID]; exists {
		c.mu.Unlock()
		cancel()
		c.close(StatusSubscriberExists, fmt.Sprintf("Subscriber for %s already exists", msg.ID))
		return false
	}
	// Over the cap the connection survives: one client asking for too much at
	// once is a fault of that operation, not of the connection.
	if c.cfg.MaxSubs > 0 && len(c.subs) >= c.cfg.MaxSubs {
		c.mu.Unlock()
		cancel()
		return c.writeError(msg.ID, graphql.Errorf("This connection allows at most %d operations at a time.", c.cfg.MaxSubs)) == nil
	}
	c.subs[msg.ID] = cancel
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.wg.Done()
		defer cancel()
		c.run(ctx, msg.ID, req)
	}()
	return true
}

// run executes one operation and streams its results.
func (c *conn) run(ctx context.Context, id string, req *graphql.Request) {
	kind, kerr := c.cfg.Exec.OperationKind(req.Query, req.OperationName)
	if kerr != nil || kind != ast.Subscription {
		c.runOnce(ctx, id, req)
		return
	}

	// A subscription also ends when the connection drains; a query or
	// mutation above does not, so a client learns its result.
	ctx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	defer context.AfterFunc(c.streams, cancelStream)()
	events, err := c.cfg.Exec.Subscribe(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			// Accepted just before a drain and cancelled by it: a terminal
			// error here would tell the client not to resubscribe.
			return
		}
		var se *graphql.SubscribeError
		if errors.As(err, &se) {
			c.finishWithErrors(id, se.Response.Errors)
			return
		}
		c.finishWithErrors(id, []*graphql.Error{graphql.Errorf("%v", err)})
		return
	}

	for resp := range events {
		// The protocol says nothing more may be sent for an id the client has
		// completed. Checking here narrows that to the one event that was
		// already mid-write, which no design can avoid.
		if ctx.Err() != nil {
			resp.Release()
			return
		}
		err := c.writeNext(id, resp)
		resp.Release()
		if err != nil {
			// Stop consuming so the executor's pump is not left producing
			// into a connection that can no longer take it.
			c.stop(id)
			return
		}
	}
	if ctx.Err() != nil {
		// Cancelled by a client complete or by the connection ending; the
		// protocol expects no further message for this id.
		return
	}
	c.finish(id, OutMessage{ID: id, Type: TypeComplete})
}

// runOnce serves a query or mutation as one next followed by complete.
func (c *conn) runOnce(ctx context.Context, id string, req *graphql.Request) {
	resp := c.cfg.Exec.Execute(ctx, req)
	defer resp.Release()

	// An operation that never ran is a protocol error, not a payload: the
	// spec reserves the error message for exactly this.
	if resp.HasRequestErrors() {
		c.finishWithErrors(id, resp.Errors)
		return
	}
	if err := c.writeNext(id, resp); err != nil {
		c.stop(id)
		return
	}
	if ctx.Err() != nil {
		return
	}
	c.finish(id, OutMessage{ID: id, Type: TypeComplete})
}

// finish sends a terminal message and retires the id, so that the client may
// immediately reuse it.
func (c *conn) finish(id string, msg OutMessage) {
	c.forget(id)
	if err := c.write(c.ctx, msg); err != nil {
		c.cfg.Logger.Debug("gqlwsproto: writing terminal message", "id", id, "error", err)
	}
}

// finishWithErrors ends an operation with an error message, which the
// protocol treats as terminal: no complete follows it.
func (c *conn) finishWithErrors(id string, errs []*graphql.Error) {
	payload, err := json.Marshal(errs)
	if err != nil {
		payload = []byte(`[{"message":"Internal server error."}]`)
	}
	c.finish(id, OutMessage{ID: id, Type: TypeError, Payload: payload})
}

func (c *conn) writeError(id string, e *graphql.Error) error {
	payload, err := json.Marshal([]*graphql.Error{e})
	if err != nil {
		return err
	}
	return c.write(c.ctx, OutMessage{ID: id, Type: TypeError, Payload: payload})
}

// writeNext frames one result. It deliberately writes under the connection
// context rather than the operation's: coder/websocket tears down the whole
// connection when a write context is cancelled mid-frame, so passing the
// operation context here would let one client unsubscribing at the wrong
// moment drop every other subscription sharing the connection.
func (c *conn) writeNext(id string, resp *graphql.Response) error {
	payload, err := resp.MarshalJSON()
	if err != nil {
		return err
	}
	return c.write(c.ctx, OutMessage{ID: id, Type: TypeNext, Payload: payload})
}

// stop cancels an operation without sending anything, for an unsubscribe or a
// write failure.
func (c *conn) stop(id string) {
	c.mu.Lock()
	cancel := c.subs[id]
	delete(c.subs, id)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// forget retires an id but leaves its context alone; the caller's deferred
// cancel handles that.
func (c *conn) forget(id string) {
	c.mu.Lock()
	delete(c.subs, id)
	c.mu.Unlock()
}

// cancelAll is deliberately redundant with c.cancel: every subscription's
// context is already derived from c.ctx (see subscribe), so cancelling the
// connection alone frees them all. This loop cancels each one explicitly too,
// so that either mechanism failing on its own still leaves the other to
// release every subscription. That redundancy is invisible to the test
// suite: breaking just this loop, or just the context derivation, still
// passes every existing test, because the other half compensates. Only
// breaking both leaks. Do not remove this loop on the grounds that c.cancel()
// already covers it -- that grounds is exactly what makes the removal unsafe.
func (c *conn) cancelAll() {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.subs))
	for id, cancel := range c.subs {
		cancels = append(cancels, cancel)
		delete(c.subs, id)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	c.cancel()
}

// startPings keeps an idle connection observably alive. It returns a stop
// function rather than relying on the connection context, so the ticker is
// released as soon as serve returns.
func (c *conn) startPings() func() {
	if c.cfg.PingInterval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(c.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := c.write(c.ctx, OutMessage{Type: TypePing}); err != nil {
					return
				}
			case <-done:
				return
			case <-c.ctx.Done():
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// read decodes one client message. A frame that is not usable ends the
// connection with StatusBadRequest, because the protocol has no way to report
// a malformed message against an operation.
func (c *conn) read(ctx context.Context) (InMessage, error) {
	// The read never ends by cancellation, only by the socket closing, which
	// every path that ends a connection already does. coder/websocket closes
	// the connection without a frame when a read's context is cancelled, so
	// reading under a cancellable context let a drain that gave up drop the
	// connection before watch could send 1001. The values are kept.
	data, err := c.sock.Read(context.WithoutCancel(ctx))
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

func (c *conn) close(code int, reason string) {
	// Close reasons are capped at 123 bytes by RFC 6455.
	if len(reason) > 123 {
		reason = reason[:123]
	}
	if err := c.sock.Close(code, reason); err != nil {
		c.cfg.Logger.Debug("gqlwsproto: closing connection", "code", code, "error", err)
	}
}
