package gqlws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
)

// conn is one WebSocket connection. Operations run on their own goroutines so
// that the read loop stays responsive: a client must be able to send complete
// to cancel a subscription that is producing continuously.
type conn struct {
	h      *Handler
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	subs map[string]context.CancelFunc

	wg sync.WaitGroup
}

func (c *conn) serve(r *http.Request) {
	if !c.handshake(r) {
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
func (c *conn) handshake(r *http.Request) bool {
	// The timeout closes the connection rather than bounding the read: a read
	// aborted by its own context leaves coder/websocket no way to send a close
	// frame, so the client would see an abnormal closure instead of 4408.
	timer := time.AfterFunc(c.h.initTimeout, func() {
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
		case typeConnectionInit:
			opCtx := c.ctx
			if c.h.onConnect != nil {
				next, cerr := c.h.onConnect(withRequest(c.ctx, r), msg.Payload)
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
			if err := c.write(c.ctx, outMessage{Type: typeConnectionAck}); err != nil {
				return false
			}
			return true
		case typePing:
			if err := c.write(ctx, outMessage{Type: typePong, Payload: msg.Payload}); err != nil {
				return false
			}
		case typePong:
			// Unsolicited pongs are allowed and carry no state.
		default:
			c.close(StatusUnauthorized, "Unauthorized")
			return false
		}
	}
}

// dispatch handles one post-handshake message, reporting whether the
// connection should continue.
func (c *conn) dispatch(msg inMessage) bool {
	switch msg.Type {
	case typePing:
		return c.write(c.ctx, outMessage{Type: typePong, Payload: msg.Payload}) == nil
	case typePong:
		return true
	case typeConnectionInit:
		c.close(StatusTooManyInitRequests, "Too many initialisation requests")
		return false
	case typeSubscribe:
		return c.subscribe(msg)
	case typeComplete:
		// The client is unsubscribing. The protocol has no acknowledgement:
		// a complete travels in each direction independently.
		c.stop(msg.ID)
		return true
	default:
		c.close(StatusBadRequest, fmt.Sprintf("Invalid message type %q", msg.Type))
		return false
	}
}

func (c *conn) subscribe(msg inMessage) bool {
	if msg.ID == "" {
		c.close(StatusBadRequest, "Subscribe message is missing an id")
		return false
	}

	var payload subscribePayload
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
	if _, exists := c.subs[msg.ID]; exists {
		c.mu.Unlock()
		cancel()
		c.close(StatusSubscriberExists, fmt.Sprintf("Subscriber for %s already exists", msg.ID))
		return false
	}
	// Over the cap the connection survives: one client asking for too much at
	// once is a fault of that operation, not of the connection.
	if c.h.maxSubs > 0 && len(c.subs) >= c.h.maxSubs {
		c.mu.Unlock()
		cancel()
		return c.writeError(msg.ID, graphql.Errorf("This connection allows at most %d operations at a time.", c.h.maxSubs)) == nil
	}
	c.subs[msg.ID] = cancel
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		c.run(ctx, msg.ID, req)
	}()
	return true
}

// run executes one operation and streams its results.
func (c *conn) run(ctx context.Context, id string, req *graphql.Request) {
	kind, kerr := c.h.exec.OperationKind(req.Query, req.OperationName)
	if kerr != nil || kind != ast.Subscription {
		c.runOnce(ctx, id, req)
		return
	}

	events, err := c.h.exec.Subscribe(ctx, req)
	if err != nil {
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
	c.finish(id, outMessage{ID: id, Type: typeComplete})
}

// runOnce serves a query or mutation as one next followed by complete.
func (c *conn) runOnce(ctx context.Context, id string, req *graphql.Request) {
	resp := c.h.exec.Execute(ctx, req)
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
	c.finish(id, outMessage{ID: id, Type: typeComplete})
}

// finish sends a terminal message and retires the id, so that the client may
// immediately reuse it.
func (c *conn) finish(id string, msg outMessage) {
	c.forget(id)
	if err := c.write(c.ctx, msg); err != nil {
		c.h.logger.Debug("gqlws: writing terminal message", "id", id, "error", err)
	}
}

// finishWithErrors ends an operation with an error message, which the
// protocol treats as terminal: no complete follows it.
func (c *conn) finishWithErrors(id string, errs []*graphql.Error) {
	payload, err := json.Marshal(errs)
	if err != nil {
		payload = []byte(`[{"message":"Internal server error."}]`)
	}
	c.finish(id, outMessage{ID: id, Type: typeError, Payload: payload})
}

func (c *conn) writeError(id string, e *graphql.Error) error {
	payload, err := json.Marshal([]*graphql.Error{e})
	if err != nil {
		return err
	}
	return c.write(c.ctx, outMessage{ID: id, Type: typeError, Payload: payload})
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
	return c.write(c.ctx, outMessage{ID: id, Type: typeNext, Payload: payload})
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

// cancelAll releases every operation on the connection.
//
// This is deliberately redundant with the operation contexts themselves: each
// derives from the connection context, so c.cancel alone would release them,
// and each stored cancel alone would too. Load testing showed that breaking
// either one leaves every test passing, including the leak tests -- only
// breaking both leaks. Keep both, and do not take a green suite as evidence
// that the one you removed was dead.
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
	if c.h.pingInterval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(c.h.pingInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := c.write(c.ctx, outMessage{Type: typePing}); err != nil {
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
func (c *conn) read(ctx context.Context) (inMessage, error) {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return inMessage{}, err
	}
	if typ != websocket.MessageText {
		c.close(StatusBadRequest, "Messages must be text frames")
		return inMessage{}, errors.New("gqlws: binary frame")
	}
	var msg inMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.close(StatusBadRequest, "Invalid JSON message")
		return inMessage{}, err
	}
	if msg.Type == "" {
		c.close(StatusBadRequest, "Message is missing a type")
		return inMessage{}, errors.New("gqlws: message without a type")
	}
	return msg, nil
}

// write sends one message. coder/websocket serializes writers internally, so
// concurrent operations may call this without further locking.
func (c *conn) write(ctx context.Context, msg outMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func (c *conn) close(code int, reason string) {
	// Close reasons are capped at 123 bytes by RFC 6455.
	if len(reason) > 123 {
		reason = reason[:123]
	}
	if err := c.ws.Close(websocket.StatusCode(code), reason); err != nil {
		c.h.logger.Debug("gqlws: closing connection", "code", code, "error", err)
	}
}
