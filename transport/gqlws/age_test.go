package gqlws_test

import (
	"testing"
	"time"

	"github.com/syssam/graphql-go/transport/gqlws"
)

// TestWithMaxConnectionAgeDrainsConnection proves the option reaches
// gqlwsproto: a live subscription gets its connection drained and closed
// 1001 once the connection has been open roughly its configured age, not
// immediately.
func TestWithMaxConnectionAgeDrainsConnection(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithMaxConnectionAge(100*time.Millisecond, 0))
	c.init("")

	c.subscribe("1", `subscription { messages { id } }`)

	start := time.Now()
	code := c.recvErr()
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("closed after %v, want not before 50ms (age is 100ms)", elapsed)
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
