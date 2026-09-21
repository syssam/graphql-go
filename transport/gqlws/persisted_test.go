package gqlws_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/ext/trusted"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// persisted builds the subscribe payload a graphql-ws client sends for an
// automatic persisted query: the hash always, the text only when registering.
func persisted(t *testing.T, query string, withText bool) json.RawMessage {
	t.Helper()
	p := map[string]any{
		"extensions": map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": apq.Hash(query),
			},
		},
	}
	if withText {
		p["query"] = query
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The full APQ handshake over one connection: a hash the server has never
// seen is a miss the client can act on, the client registers by resending
// with the text, and the bare hash then works.
func TestPersistedQueryRoundTripOverWebSocket(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithPersistedQueries(apq.NewCache(16)))
	c.init("{}")

	const query = `{ ping }`

	// Miss. It must arrive as next+complete, not as error: an error message
	// is terminal for graphql-ws and the client would never retry.
	c.send(frame{ID: "1", Type: "subscribe", Payload: persisted(t, query, false)})
	got := c.recv()
	if got.Type != "next" {
		t.Fatalf("miss arrived as %q, want next (an error is terminal and kills the retry)", got.Type)
	}
	if !strings.Contains(string(got.Payload), "PersistedQueryNotFound") {
		t.Fatalf("miss payload = %s, want PersistedQueryNotFound", got.Payload)
	}
	if done := c.recv(); done.Type != "complete" {
		t.Fatalf("after the miss: %q, want complete", done.Type)
	}

	// Register by resending with the text.
	c.send(frame{ID: "2", Type: "subscribe", Payload: persisted(t, query, true)})
	if got := c.recv(); got.Type != "next" || !strings.Contains(string(got.Payload), "pong") {
		t.Fatalf("registration = %q %s, want next carrying pong", got.Type, got.Payload)
	}
	if done := c.recv(); done.Type != "complete" {
		t.Fatalf("after registration: %q, want complete", done.Type)
	}

	// The bare hash now runs.
	c.send(frame{ID: "3", Type: "subscribe", Payload: persisted(t, query, false)})
	if got := c.recv(); got.Type != "next" || !strings.Contains(string(got.Payload), "pong") {
		t.Fatalf("registered hash = %q %s, want next carrying pong", got.Type, got.Payload)
	}
	if done := c.recv(); done.Type != "complete" {
		t.Fatalf("after the registered hash: %q, want complete", done.Type)
	}
}

// Registration is verified, not trusted: a server that stored whatever text
// arrived beside a hash would let one client choose what every later client's
// hash executes.
func TestPersistedQueryRejectsAMismatchedHash(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithPersistedQueries(apq.NewCache(16)))
	c.init("{}")

	payload, err := json.Marshal(map[string]any{
		"query": `{ ping }`,
		"extensions": map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": apq.Hash(`{ whoami }`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.send(frame{ID: "1", Type: "subscribe", Payload: payload})
	got := c.recv()
	if !strings.Contains(string(got.Payload), "does not match") && !strings.Contains(string(got.Payload), "Mismatch") {
		t.Fatalf("payload = %s, want a hash-mismatch refusal", got.Payload)
	}
}

// A safelist must refuse query text however it hashes, and must refuse a
// freeform request carrying no hash at all, which is otherwise the way
// straight round it. ext/trusted is an apq.Cache, so the same option wires it.
func TestTrustedStoreOverWebSocketRefusesFreeform(t *testing.T) {
	_, e := newTestExecutor(t)
	store := trusted.NewStore(map[string]string{"known": `{ ping }`})
	c := dial(t, e, gqlws.WithPersistedQueries(store))
	c.init("{}")

	// Freeform text with no hash at all.
	c.send(frame{ID: "1", Type: "subscribe", Payload: json.RawMessage(`{"query":"{ ping }"}`)})
	got := c.recv()
	if got.Type == "next" && strings.Contains(string(got.Payload), "pong") {
		t.Fatalf("a safelist ran freeform query text: %s", got.Payload)
	}
	// The refusal is a result, so it is next followed by complete. Drain the
	// complete or it is read as the next operation's reply.
	if got.Type == "next" {
		if done := c.recv(); done.Type != "complete" {
			t.Fatalf("after the refusal: %q, want complete", done.Type)
		}
	}

	// The registered id runs.
	c.send(frame{ID: "2", Type: "subscribe", Payload: json.RawMessage(
		`{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"known"}}}`)})
	if got := c.recv(); got.Type != "next" || !strings.Contains(string(got.Payload), "pong") {
		t.Fatalf("registered document = %q %s, want next carrying pong", got.Type, got.Payload)
	}
}

// Without the option, a subscribe carrying a persistedQuery extension and no
// query text is an ordinary missing-query request, exactly as before.
func TestWithoutTheOptionPersistedIsNotResolved(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("{}")

	c.send(frame{ID: "1", Type: "subscribe", Payload: persisted(t, `{ ping }`, false)})
	if got := c.recv(); got.Type == "next" && strings.Contains(string(got.Payload), "pong") {
		t.Fatalf("a persisted hash ran with no cache configured: %s", got.Payload)
	}
}
