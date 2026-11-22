package gqlws_test

import (
	"testing"
	"time"

	"github.com/syssam/graphql-go/transport/gqlws"
)

// TestWithMaxConnectionAgeDrainsConnection proves the option reaches
// gqlwsproto: a connection with a subscription sent gets drained and closed
// 1001 once the connection has been open roughly its configured age, not
// immediately.
func TestWithMaxConnectionAgeDrainsConnection(t *testing.T) {
	_, e := newTestExecutor(t)
	// The age runs from when the server accepts the connection, so the clock
	// starts before the dial: started after the subscription registered, it
	// missed however long dial, init and subscribe took, and under CI load
	// that was 60 ms of the 100. A timer never fires early, so the close
	// comes no sooner than the age less its 10% jitter.
	start := time.Now()
	c := dial(t, e, gqlws.WithMaxConnectionAge(100*time.Millisecond, 0))
	c.init("")

	c.subscribe("1", `subscription { messages { id } }`)

	code := c.recvErr()
	elapsed := time.Since(start)

	if elapsed < 90*time.Millisecond {
		t.Fatalf("closed %v after the dial, want not before 90ms (age is 100ms, give or take 10%%)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("closed after %v, want within 2s", elapsed)
	}
	if code != gqlws.StatusGoingAway {
		t.Fatalf("close code = %d, want %d", code, gqlws.StatusGoingAway)
	}
}

// TestWithMaxConnectionIdleClosesConnection proves the option reaches
// gqlwsproto: a connection with no operation in flight closes 1000 once it
// has been idle roughly its configured period.
func TestWithMaxConnectionIdleClosesConnection(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithMaxConnectionIdle(100*time.Millisecond))
	c.init("")

	start := time.Now()
	code := c.recvErr()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("closed after %v, want within 2s", elapsed)
	}
	if code != gqlws.StatusNormalClosure {
		t.Fatalf("close code = %d, want %d", code, gqlws.StatusNormalClosure)
	}
}
