package gqlfiber

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/ext/apq"
)

// gqlfiber and gqlws drive the same gqlwsproto, but the wiring that reaches it
// is per transport, so prove this driver reaches it too rather than assuming
// the shared core is enough.
func TestPersistedQueryOverFiberWebSocket(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithPersistedQueries(apq.NewCache(16)))
	c.init()

	const query = `{ hello }`
	hashOnly, err := json.Marshal(map[string]any{
		"extensions": map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": apq.Hash(query)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A miss must be next+complete, not error: error is terminal for
	// graphql-ws and the client would never retry with the text.
	c.send(wsFrame{ID: "1", Type: "subscribe", Payload: hashOnly})
	got := c.recv()
	if got.Type != "next" || !strings.Contains(string(got.Payload), "PersistedQueryNotFound") {
		t.Fatalf("miss = %q %s, want next carrying PersistedQueryNotFound", got.Type, got.Payload)
	}
	if done := c.recv(); done.Type != "complete" {
		t.Fatalf("after the miss: %q, want complete", done.Type)
	}

	// Register, then the bare hash runs.
	withText, err := json.Marshal(map[string]any{
		"query": query,
		"extensions": map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": apq.Hash(query)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.send(wsFrame{ID: "2", Type: "subscribe", Payload: withText})
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("registration = %q %s", got.Type, got.Payload)
	}
	if done := c.recv(); done.Type != "complete" {
		t.Fatalf("after registration: %q, want complete", done.Type)
	}

	c.send(wsFrame{ID: "3", Type: "subscribe", Payload: hashOnly})
	if got := c.recv(); got.Type != "next" || !strings.Contains(string(got.Payload), "world") {
		t.Fatalf("registered hash = %q %s, want next carrying pong", got.Type, got.Payload)
	}
}
