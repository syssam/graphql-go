package gqlsse_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/syssam/graphql-go/transport/gqlsse"
)

// TestMaxStreamAgeEndsSubscriptionWithoutComplete proves the option reaches
// the subscription loop: a stream with a source that never produces an event
// is ended by the age timer alone, and it ends the way a drain does -- no
// complete event, so the client reconnects instead of treating the
// subscription as finished for good.
func TestMaxStreamAgeEndsSubscriptionWithoutComplete(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e,
		gqlsse.WithCSRFPrevention(false), gqlsse.WithMaxStreamAge(100*time.Millisecond))

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	done := make(chan []event, 1)
	go func() { done <- readEvents(t, resp.Body) }()

	select {
	case got := <-done:
		for _, ev := range got {
			if ev.name == "complete" {
				t.Fatalf("stream carried a complete event: %+v", got)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end within 2s of a 100ms WithMaxStreamAge")
	}
}

// TestMaxStreamAgeLeavesSingleResultAlone: a query or mutation is one next
// then complete before the age timer could ever matter, so the option must
// not touch that path.
func TestMaxStreamAgeLeavesSingleResultAlone(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e,
		gqlsse.WithCSRFPrevention(false), gqlsse.WithMaxStreamAge(100*time.Millisecond))

	resp := post(t, client, srv.URL, `{"query":"{ ping }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := readEvents(t, resp.Body)
	want := []event{
		{"next", `{"data":{"ping":"pong"}}`},
		{"complete", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
