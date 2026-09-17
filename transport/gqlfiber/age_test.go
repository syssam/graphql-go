package gqlfiber

import (
	"testing"
	"time"
)

// TestWSWithMaxConnectionAgeDrainsConnection proves the option reaches
// gqlwsproto: a live subscription gets its connection drained and closed
// 1001 once the connection has been open roughly its configured age, not
// immediately.
func TestWSWithMaxConnectionAgeDrainsConnection(t *testing.T) {
	src := newIdleWSSource()
	c := dialWS(t, idleExecutor(t, src), WithMaxConnectionAge(100*time.Millisecond, 0))
	c.init()

	c.subscribe("1", `subscription { ticks }`)
	waitRegistered(t, src, 1)

	start := time.Now()
	code := c.recvClose()
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("closed after %v, want not before 50ms (age is 100ms)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("closed after %v, want within 2s", elapsed)
	}
	if code != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
	}
}

// TestWSWithMaxConnectionIdleClosesConnection proves the option reaches
// gqlwsproto: a connection with no operation in flight closes 1000 once it
// has been idle roughly its configured period.
func TestWSWithMaxConnectionIdleClosesConnection(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithMaxConnectionIdle(100*time.Millisecond))
	c.init()

	start := time.Now()
	code := c.recvClose()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("closed after %v, want within 2s", elapsed)
	}
	if code != StatusNormalClosure {
		t.Fatalf("close code = %d, want %d", code, StatusNormalClosure)
	}
}
