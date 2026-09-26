package gqlwsproto

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// blockFirstWrite holds the first write (connection_ack) until release is
// closed, the way a client that is slow to read holds a real one.
type blockFirstWrite struct {
	*fakeSocket
	release chan struct{}
	first   chan struct{}
}

func (b *blockFirstWrite) Write(ctx context.Context, data []byte) error {
	select {
	case <-b.first:
	default:
		close(b.first)
		<-b.release
	}
	return b.fakeSocket.Write(ctx, data)
}

// Once the ack is decided the init timeout must not close the connection.
// The timer used to be stopped only by handshake's defer, so one firing
// while the ack was still being written closed an authenticated connection
// with 4408.
func TestInitTimeoutAfterAckDoesNotClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sock := &blockFirstWrite{fakeSocket: newFakeSocket(), release: make(chan struct{}), first: make(chan struct{})}
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{InitTimeout: time.Second, MaxSubs: 10})

		<-sock.first
		time.Sleep(2 * time.Second) // the timeout passes while the ack write is blocked
		close(sock.release)
		synctest.Wait()

		if code := sock.closeCode(); code != 0 {
			t.Fatalf("acknowledged connection closed with %d", code)
		}
		if got := sock.types(); len(got) != 1 || got[0] != "connection_ack" {
			t.Fatalf("messages = %v, want one connection_ack", got)
		}
		_ = sock.Close(1000, "done")
		<-done
	})
}

// A timeout that fired during a slow OnConnect has already closed the socket
// 4408, so the handshake must not go on to acknowledge and serve it.
func TestInitTimeoutDuringOnConnectIsNotAcknowledged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{
			InitTimeout: time.Second, MaxSubs: 10,
			OnConnect: func(ctx context.Context, _ []byte) (context.Context, error) {
				time.Sleep(2 * time.Second)
				return nil, nil
			},
		})
		<-done

		if code := sock.closeCode(); code != StatusInitTimeout {
			t.Fatalf("close code = %d, want %d", code, StatusInitTimeout)
		}
		if got := sock.types(); len(got) != 0 {
			t.Fatalf("messages = %v, want none after the timeout", got)
		}
	})
}

// The contract on its own, without a connection around it: whichever of the
// timer and the ack takes the lock first wins, and the loser does nothing.
func TestInitDeadlineAckAndFireExclude(t *testing.T) {
	expired := 0
	g := newInitDeadline(time.Hour, func() { expired++ })
	if !g.ack() {
		t.Fatal("ack refused before any timeout")
	}
	g.fire(func() { expired++ })
	if expired != 0 {
		t.Fatal("timeout ran after the ack")
	}

	g = newInitDeadline(time.Hour, func() {})
	g.fire(func() { expired++ })
	if expired != 1 {
		t.Fatalf("timeout ran %d times, want 1", expired)
	}
	if g.ack() {
		t.Fatal("ack allowed after the timeout closed the connection")
	}
	g.stop()

	var none *initDeadline
	if !none.ack() {
		t.Fatal("no timeout configured, but ack refused")
	}
}
