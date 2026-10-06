package gqlsse_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

func quietSubscription(t *testing.T) (*graphql.Executor, chan int) {
	t.Helper()
	ticks := make(chan int)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { ping: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) { return ticks, nil })),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return graphql.NewExecutor(s), ticks
}

func openStream(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"query":"subscription { tick }"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return resp
}

// The write timeout bounds a write. Left set after the write, it is a deadline
// on the stream: on HTTP/2 the server resets a stream whose write deadline
// passes whether or not anything is being written, so a subscription with
// nothing to say for longer than the timeout was cut off.
func TestTheWriteTimeoutDoesNotEndAQuietStream(t *testing.T) {
	for _, proto := range []string{"HTTP/1.1", "HTTP/2"} {
		t.Run(proto, func(t *testing.T) {
			e, ticks := quietSubscription(t)
			srv := httptest.NewUnstartedServer(gqlsse.New(e,
				gqlsse.WithWriteTimeout(100*time.Millisecond), gqlsse.WithKeepAlive(0)))
			if proto == "HTTP/2" {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()

			resp := openStream(t, srv.Client(), srv.URL)
			// Deferred after srv.Close, so it runs first: Close waits for
			// open streams, and this one ends only when its client leaves.
			defer func() { _ = resp.Body.Close() }()
			if proto == "HTTP/2" && resp.ProtoMajor != 2 {
				t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
			}
			// Quiet for several timeouts, then one event.
			time.Sleep(400 * time.Millisecond)
			go func() { ticks <- 7 }()

			lines := make(chan string, 1)
			go func() {
				r := bufio.NewReader(resp.Body)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						lines <- "read error: " + err.Error()
						return
					}
					if strings.HasPrefix(line, "data: ") {
						lines <- strings.TrimSpace(line)
						return
					}
				}
			}()
			select {
			case got := <-lines:
				if got != `data: {"data":{"tick":7}}` {
					t.Fatalf("after a quiet spell the stream gave %q, want the event", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event arrived")
			}
		})
	}
}

// And a stream that ends after a quiet spell -- by its age limit here -- ends
// as a complete response. With the deadline of an earlier write still set, the
// terminating chunk was written past it and failed, so the client read a
// truncated body.
func TestAStreamEndsCleanlyAfterAQuietSpell(t *testing.T) {
	e, _ := quietSubscription(t)
	srv := httptest.NewServer(gqlsse.New(e, gqlsse.WithWriteTimeout(100*time.Millisecond),
		gqlsse.WithKeepAlive(0), gqlsse.WithMaxStreamAge(400*time.Millisecond)))
	defer srv.Close()

	resp := openStream(t, srv.Client(), srv.URL)
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("the stream did not end as a complete response: %v", err)
	}
}

// http.Server.WriteTimeout is one deadline for a whole response, which a
// subscription outlives by design. The handler's own deadline replaces it on
// the connection before each write and is taken off after, so with the two set
// together the stream goes on past the server's; with the handler's turned
// off nothing replaces the server's, and it ends the stream.
func TestTheServersWriteTimeoutAndAStream(t *testing.T) {
	const serverTimeout = 150 * time.Millisecond
	open := func(t *testing.T, opts ...gqlsse.Option) (*bufio.Reader, chan int) {
		t.Helper()
		e, ticks := quietSubscription(t)
		srv := httptest.NewUnstartedServer(gqlsse.New(e, append(opts, gqlsse.WithKeepAlive(0))...))
		srv.Config.WriteTimeout = serverTimeout
		srv.Start()
		t.Cleanup(srv.Close)
		resp := openStream(t, srv.Client(), srv.URL)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return bufio.NewReader(resp.Body), ticks
	}
	// readTick reports whether the next event on the stream carries n.
	readTick := func(br *bufio.Reader, n string) bool {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return false
			}
			if strings.HasPrefix(line, "data: ") {
				return strings.Contains(line, `"tick":`+n)
			}
		}
	}
	send := func(t *testing.T, ticks chan int, n int) {
		t.Helper()
		select {
		case ticks <- n:
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d was never taken", n)
		}
	}

	t.Run("a stream with its own write timeout outlives it", func(t *testing.T) {
		br, ticks := open(t, gqlsse.WithWriteTimeout(5*time.Second))
		send(t, ticks, 1)
		if !readTick(br, "1") {
			t.Fatal("the first event did not arrive")
		}
		time.Sleep(3 * serverTimeout)
		send(t, ticks, 2)
		if !readTick(br, "2") {
			t.Fatal("the stream was ended at the server's WriteTimeout")
		}
	})

	t.Run("a stream without one is ended by it", func(t *testing.T) {
		br, ticks := open(t, gqlsse.WithWriteTimeout(0))
		send(t, ticks, 1)
		if !readTick(br, "1") {
			t.Fatal("the first event did not arrive")
		}
		time.Sleep(3 * serverTimeout)
		send(t, ticks, 2)
		if readTick(br, "2") {
			t.Fatal("an event arrived past the server's WriteTimeout with nothing to replace it")
		}
	})
}

// deadlineRecorder is a ResponseWriter that keeps, in order, every write
// deadline set on it, every write and every flush. Consecutive writes are one
// entry: how many an event takes is the framing's business.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu  sync.Mutex
	ops []string
}

func (d *deadlineRecorder) record(op string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if op != "write" || len(d.ops) == 0 || d.ops[len(d.ops)-1] != "write" {
		d.ops = append(d.ops, op)
	}
}

func (d *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	if at.IsZero() {
		d.record("off")
	} else {
		d.record("set")
	}
	return nil
}

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	d.record("write")
	return d.ResponseRecorder.Write(p)
}

func (d *deadlineRecorder) WriteString(s string) (int, error) {
	d.record("write")
	return d.ResponseRecorder.WriteString(s)
}

func (d *deadlineRecorder) Flush() {
	d.record("flush")
	d.ResponseRecorder.Flush()
}

// Every event is written under a deadline, and under one: set before its
// first byte, covering its flush, and taken off after. An event is five
// writes, and each used to set the deadline again; an event written with none
// is one a stalled reader holds the handler in for good.
func TestEachEventIsWrittenUnderOneDeadline(t *testing.T) {
	event := []string{"set", "write", "flush", "off"}

	t.Run("a subscription", func(t *testing.T) {
		e, ticks := quietSubscription(t)
		h := gqlsse.New(e, gqlsse.WithKeepAlive(0))
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"query":"subscription { tick }"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "text/event-stream")
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(w, r)
		}()
		ticks <- 1
		ticks <- 2
		close(ticks)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end with its source")
		}

		// The response head, two events and the complete.
		want := slices.Concat([]string{"set", "flush", "off"}, event, event, event)
		if !slices.Equal(w.ops, want) {
			t.Errorf("ops = %v\nwant  %v", w.ops, want)
		}
		if body := w.Body.String(); strings.Count(body, "event: next") != 2 || !strings.HasSuffix(body, "event: complete\ndata:\n\n") {
			t.Errorf("stream = %q", body)
		}
	})

	t.Run("a query", func(t *testing.T) {
		e, _ := quietSubscription(t)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"query":"{ ping }"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "text/event-stream")
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		gqlsse.New(e).ServeHTTP(w, r)

		// Its next and its complete go out together, under the one deadline.
		if !slices.Equal(w.ops, event) {
			t.Errorf("ops = %v, want %v", w.ops, event)
		}
	})
}
