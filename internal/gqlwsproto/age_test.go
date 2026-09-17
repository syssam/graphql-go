package gqlwsproto

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func serveWith(sock Socket, cfg Config) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, cfg)
	}()
	return done
}

func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func wrote(sock *fakeSocket, s string) bool {
	sock.mu.Lock()
	defer sock.mu.Unlock()
	for _, b := range sock.out {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

const (
	initMsg      = `{"type":"connection_init"}`
	subscribeSub = `{"id":"1","type":"subscribe","payload":{"query":"subscription { tick }"}}`
	subscribeQry = `{"id":"q","type":"subscribe","payload":{"query":"{ block }"}}`
)

// TestMaxConnectionAgeDrains: an age limit ends a connection within ±10% of
// the age, as a drain — the subscription gets no complete, which would tell
// the client not to resubscribe once it reconnects.
func TestMaxConnectionAgeDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionAge: time.Hour})

		// Without a live subscription the no-complete check below would pass
		// for a subscription that never opened.
		src.ticks <- 1
		synctest.Wait()
		if !slices.Contains(sock.types(), "next") {
			t.Fatalf("subscription is not live: %v", sock.types())
		}

		time.Sleep(53 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d before 90%% of its age", code)
		}
		time.Sleep(14 * time.Minute)
		synctest.Wait()
		if !isDone(done) {
			t.Fatal("still open past 110% of its age")
		}
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
		}
		if slices.Contains(sock.types(), "complete") {
			t.Fatalf("age drain sent complete: %v", sock.types())
		}
	})
}

// TestMaxConnectionAgeLetsQueryFinish: inside its grace a query in flight
// finishes and its client learns the result before the close.
func TestMaxConnectionAgeLetsQueryFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10,
			MaxConnectionAge: time.Hour, MaxConnectionAgeGrace: time.Hour})

		<-src.entered
		time.Sleep(67 * time.Minute)
		synctest.Wait()
		if isDone(done) || sock.closeCode() != 0 {
			t.Fatal("closed while a query was running inside its grace")
		}
		close(src.release)
		<-done
		got := sock.types()
		if n := len(got); n < 2 || got[n-2] != "next" || got[n-1] != "complete" {
			t.Fatalf("message types = %v, want the query's next and complete last", got)
		}
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
		}
	})
}

// TestMaxConnectionAgeGraceCuts: past its grace the connection closes even
// with an operation still running.
func TestMaxConnectionAgeGraceCuts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10,
			MaxConnectionAge: time.Hour, MaxConnectionAgeGrace: 10 * time.Minute})

		<-src.entered
		time.Sleep(77 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d past age plus grace, want %d", code, StatusGoingAway)
		}
		close(src.release)
		<-done
	})
}

// TestMaxConnectionAgeRefusesNewOperations: once the age drain has begun a
// new operation is refused with the age's own message.
func TestMaxConnectionAgeRefusesNewOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionAge: time.Hour})

		<-src.entered
		time.Sleep(67 * time.Minute)
		synctest.Wait()
		sock.in <- []byte(subscribeSub)
		synctest.Wait()
		if !wrote(sock, "The connection has reached its maximum age.") {
			t.Fatalf("no age refusal written: %v", sock.types())
		}
		close(src.release)
		<-done
	})
}

// TestMaxConnectionIdleCloses: with nothing in flight the connection closes
// normally after the idle period, not before.
func TestMaxConnectionIdleCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute})

		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d before the idle period", code)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d after the idle period, want %d", code, StatusNormalClosure)
		}
		<-done
	})
}

// TestMaxConnectionIdleWaitsForOperations: an open subscription is not idle,
// and the idle period starts again when the last operation ends.
func TestMaxConnectionIdleWaitsForOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute})

		time.Sleep(30 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d while a subscription was open", code)
		}
		sock.in <- []byte(`{"id":"1","type":"complete"}`)
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d less than the idle period after the last operation ended", code)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d", code, StatusNormalClosure)
		}
		<-done
	})
}

// TestNoLimitsKeepsConnectionOpen guards the defaults.
func TestNoLimitsKeepsConnectionOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10})

		time.Sleep(100 * time.Hour)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d with no limits configured", code)
		}
		_ = sock.Close(1000, "test done")
		<-done
	})
}

// TestMaxConnectionIdleIgnoresUnknownComplete: a complete for an id that was
// never running must not restart the idle period, or a client could hold an
// idle connection open by sending them.
func TestMaxConnectionIdleIgnoresUnknownComplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute})

		time.Sleep(9 * time.Minute)
		sock.in <- []byte(`{"id":"zz","type":"complete"}`)
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d", code, StatusNormalClosure)
		}
		<-done
	})
}
