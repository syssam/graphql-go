// Package drain winds down long-lived connections when a server shuts down.
//
// net/http's Server.Shutdown and fasthttp's ShutdownWithContext wait for
// ordinary requests, but not for hijacked WebSockets, and an SSE subscription
// never ends on its own, so Shutdown waits out its whole deadline for one.
// A Drain is handed to every handler that serves such connections (see
// gqlws.WithDrain, gqlsse.WithDrain and gqlfiber.WithDrain) and shut down
// alongside the server:
//
//	d := drain.New()
//	mux.Handle("/graphql/ws", gqlws.New(exec, gqlws.WithDrain(d)))
//	...
//	var wg sync.WaitGroup
//	wg.Go(func() { _ = d.Shutdown(ctx) })
//	_ = srv.Shutdown(ctx)
//	wg.Wait()
//
// The two run together because srv.Shutdown waits for SSE handlers, which
// return only once the drain has ended their streams.
package drain

import (
	"context"
	"sync"
)

// Drain tracks the long-lived connections of one server. A nil *Drain is
// valid and does nothing, which is what a handler without the option holds.
type Drain struct {
	mu       sync.Mutex
	draining bool
	closing  chan struct{}
	wg       sync.WaitGroup

	// force is cancelled when Shutdown gives up, and every entered context
	// follows it.
	force  context.Context
	giveUp context.CancelFunc
}

// New returns a Drain that admits connections until Shutdown is called.
func New() *Drain {
	force, giveUp := context.WithCancel(context.Background())
	return &Drain{closing: make(chan struct{}), force: force, giveUp: giveUp}
}

// Enter registers one long-lived connection or stream. The returned context
// derives from parent and is also cancelled if Shutdown's deadline passes;
// leave must be called when the connection ends, and calling it again does
// nothing. Once draining has begun Enter reports ok false, hands back parent
// and a no-op leave, and the caller must refuse the connection.
func (d *Drain) Enter(parent context.Context) (ctx context.Context, leave func(), ok bool) {
	if d == nil {
		return parent, func() {}, true
	}
	// The draining check and the Add share the lock with Shutdown's flip, so
	// no Add can race the Wait that follows it.
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return parent, func() {}, false
	}
	d.wg.Add(1)
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(d.force, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
			d.wg.Done()
		})
	}, true
}

// Closing is closed when Shutdown begins. It is nil for a nil Drain, which a
// select never chooses.
func (d *Drain) Closing() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.closing
}

// Shutdown stops admitting connections, signals Closing, and waits for every
// entered connection to leave. If ctx ends first it cancels every entered
// context and returns ctx.Err() at once rather than waiting further: a wait
// that outlives its deadline is the failure this exists to prevent. The wait
// for connections that have not yet left carries on in the background after
// that return, so a handler must turn a cancelled context into a prompt leave;
// one that never does keeps that wait alive. Shutdown may be called more than
// once and concurrently.
func (d *Drain) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.draining {
		d.draining = true
		close(d.closing)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		d.giveUp()
		return ctx.Err()
	}
}
