package gqlsse_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

// TestDrainEndsStreamWithoutComplete proves WithDrain reaches an open
// subscription stream: Shutdown ends it without a complete event, so the
// client's reconnect logic (not its "the subscription ended for good" logic)
// is what fires.
func TestDrainEndsStreamWithoutComplete(t *testing.T) {
	src, e := newTestExecutor(t)
	d := drain.New()
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false), gqlsse.WithDrain(d))

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	src.messages <- &message{ID: "1"}

	// Read the first "next" event synchronously, so Shutdown is only issued
	// once the event is known to be on the wire -- otherwise the event and
	// the drain's Closing channel race in the handler's select, and either
	// might win.
	sc := bufio.NewScanner(resp.Body)
	var firstEvent string
	for sc.Scan() {
		line := sc.Text()
		firstEvent += line + "\n"
		if line == "" {
			break
		}
	}
	if !strings.Contains(firstEvent, `"messages":{"id":"1"}`) {
		t.Fatalf("first event did not carry the published message: %q", firstEvent)
	}

	type result struct {
		err error
		dur time.Duration
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := d.Shutdown(ctx)
		done <- result{err, time.Since(start)}
	}()

	rest := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		rest <- b
	}()

	var read []byte
	select {
	case read = <-rest:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the stream to EOF")
	}

	if strings.Contains(string(read), "event: complete") {
		t.Fatalf("stream carried a complete event: %s", read)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Shutdown returned %v", r.err)
		}
		if r.dur > 2*time.Second {
			t.Fatalf("Shutdown took %v, want under 2s", r.dur)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

// TestDrainLetsServerShutdownReturn is the user-visible point of the
// feature: without the drain, an open SSE subscription would never end on
// its own, and http.Server.Shutdown would wait out its whole deadline for
// it. With the drain, both Shutdowns return together, well inside it.
func TestDrainLetsServerShutdownReturn(t *testing.T) {
	src, e := newTestExecutor(t)
	d := drain.New()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: gqlsse.New(e, gqlsse.WithCSRFPrevention(false), gqlsse.WithDrain(d))}
	go srv.Serve(ln)

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	url := "http://" + ln.Addr().String()
	resp := post(t, client, url, `{"query":"subscription { messages { id } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	src.messages <- &message{ID: "1"}
	buf := make([]byte, 512)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading the first event: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var drainErr, srvErr error
	start := time.Now()
	wg.Go(func() { drainErr = d.Shutdown(ctx) })
	wg.Go(func() { srvErr = srv.Shutdown(ctx) })
	wg.Wait()
	dur := time.Since(start)

	if drainErr != nil {
		t.Fatalf("drain Shutdown returned %v", drainErr)
	}
	if srvErr != nil {
		t.Fatalf("srv.Shutdown returned %v", srvErr)
	}
	if dur > 2*time.Second {
		t.Fatalf("Shutdown took %v, want under 2s", dur)
	}
}

// TestDrainRefusesNewSubscription proves the ServeHTTP entry point itself
// refuses a new subscription once draining has begun.
func TestDrainRefusesNewSubscription(t *testing.T) {
	_, e := newTestExecutor(t)
	d := drain.New()
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false), gqlsse.WithDrain(d))

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestDrainLeavesSingleResultAlone proves a query is the server's own
// Shutdown to drain, not the Drain's: it still gets its ordinary next then
// complete after draining has begun.
func TestDrainLeavesSingleResultAlone(t *testing.T) {
	_, e := newTestExecutor(t)
	d := drain.New()
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false), gqlsse.WithDrain(d))

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	resp := post(t, client, srv.URL, `{"query":"{ ping }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := readEvents(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if got[0].name != "next" || got[0].data != `{"data":{"ping":"pong"}}` {
		t.Fatalf("first event = %+v", got[0])
	}
	if got[1].name != "complete" {
		t.Fatalf("second event = %+v", got[1])
	}
}
