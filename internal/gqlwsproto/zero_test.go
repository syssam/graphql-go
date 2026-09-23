package gqlwsproto

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Zero means "no limit" for every other duration and count in Config.
// time.AfterFunc(0, ...) fires at once instead, so a zero InitTimeout closed
// every connection before its connection_init could arrive.
func TestServeZeroInitTimeoutWaitsForInit(t *testing.T) {
	sock := newFakeSocket()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{MaxSubs: 10})
	}()

	time.Sleep(50 * time.Millisecond)
	sock.in <- []byte(`{"type":"connection_init"}`)
	deadline := time.Now().Add(2 * time.Second)
	for !wrote(sock, "connection_ack") {
		if time.Now().After(deadline) {
			t.Fatalf("no connection_ack; close code %d, messages %v", sock.closeCode(), sock.types())
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = sock.Close(1000, "done")
	<-done
}

// RFC 6455 caps a close reason at 123 bytes and requires it to be UTF-8. A
// byte cut through a multi-byte rune left an invalid reason, which a
// conforming peer answers by failing the connection instead of reading it.
func TestCloseReasonIsCutOnARuneBoundary(t *testing.T) {
	for _, reason := range []string{
		strings.Repeat("é", 100),
		strings.Repeat("a", 122) + "é",
		strings.Repeat("a", 200),
		"short",
	} {
		got := closeReason(reason)
		if len(got) > 123 || !utf8.ValidString(got) || !strings.HasPrefix(reason, got) {
			t.Errorf("closeReason(%d bytes) = %d bytes, valid UTF-8 %v", len(reason), len(got), utf8.ValidString(got))
		}
		if len(reason) <= 123 && got != reason {
			t.Errorf("closeReason cut a reason that fit: %q", got)
		}
	}
}
