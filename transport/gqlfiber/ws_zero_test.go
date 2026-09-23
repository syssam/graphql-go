package gqlfiber

import (
	"testing"
	"time"
)

// Zero disables every other limit on this handler. A zero write timeout
// became time.Now().Add(0), a deadline already passed, so the first write —
// connection_ack — failed and the connection closed.
func TestWSZeroTimeoutsDisableRatherThanExpire(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithWriteTimeout(0), WithInitTimeout(0))
	time.Sleep(50 * time.Millisecond)
	c.init()
	c.subscribe("1", `{ hello }`)
	if got := c.recv(); got.Type != "next" || string(got.Payload) != `{"data":{"hello":"world"}}` {
		t.Fatalf("first frame = %+v", got)
	}
}
