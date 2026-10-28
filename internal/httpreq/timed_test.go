package httpreq

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// timedLog is the stream a TimedWriter writes to and the deadline it sets,
// recording the order the two were used in.
type timedLog struct {
	ops  []string
	data bytes.Buffer
	// failAt fails the write that would take data past this many bytes.
	failAt int
}

func (l *timedLog) arm() { l.ops = append(l.ops, "arm") }

func (l *timedLog) flush() error {
	l.ops = append(l.ops, "flush")
	return nil
}

func (l *timedLog) Write(p []byte) (int, error) {
	if l.failAt > 0 && l.data.Len()+len(p) > l.failAt {
		return 0, io.ErrClosedPipe
	}
	l.ops = append(l.ops, "bytes")
	return l.data.Write(p)
}

// stringLog is a timedLog that takes a string as it is, as a bufio.Writer and
// net/http's response do.
type stringLog struct{ timedLog }

func (l *stringLog) WriteString(s string) (int, error) {
	l.ops = append(l.ops, "string")
	return l.data.WriteString(s)
}

// countingArms measures what a deadline is for: most is the most that went
// out under any one of them.
type countingArms struct {
	arms, since, most int
}

func (c *countingArms) arm() { c.arms, c.since = c.arms+1, 0 }

func (c *countingArms) Write(p []byte) (int, error) {
	c.since += len(p)
	c.most = max(c.most, c.since)
	return len(p), nil
}

// An event is a frame, a payload and a terminator, each a write of its own,
// and one deadline bounds all of them. Setting it before every write was the
// cost this type exists to avoid.
func TestTimedWriterSetsOneDeadlineForASmallEvent(t *testing.T) {
	var log stringLog
	w := NewTimedWriter(&log, log.arm, log.flush)

	for range 2 {
		w.Arm()
		_, _ = io.WriteString(w, "event: next\ndata: ")
		_, _ = w.Write([]byte(`{"data":{"hello":"world"}}`))
		_, _ = io.WriteString(w, "\n\n")
	}

	event := []string{"arm", "string", "bytes", "string"}
	if want := slices.Concat(event, event); !slices.Equal(log.ops, want) {
		t.Errorf("ops = %v, want %v", log.ops, want)
	}
	const stream = "event: next\ndata: {\"data\":{\"hello\":\"world\"}}\n\n"
	if got := log.data.String(); got != stream+stream {
		t.Errorf("stream = %q", got)
	}
}

// The promise a client can observe: never more than a piece under one
// deadline, however the payload is cut into writes, so a reader is asked only
// to keep taking something.
func TestTimedWriterNeverWritesMoreThanAPieceUnderOneDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []int
		arms   int
	}{
		{"less than a piece", []int{timedPiece - 1}, 1},
		{"exactly a piece", []int{timedPiece}, 1},
		{"a byte more than a piece", []int{timedPiece + 1}, 2},
		{"a piece, then a byte in a second write", []int{timedPiece, 1}, 2},
		{"a frame, then three pieces", []int{18, 3 * timedPiece}, 4},
		{"many writes that add up to two pieces and a bit", slices.Repeat([]int{4096}, 17), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c countingArms
			w := NewTimedWriter(&c, c.arm, nil)
			w.Arm()
			for _, n := range tc.writes {
				if got, err := w.Write(make([]byte, n)); got != n || err != nil {
					t.Fatalf("Write(%d bytes) = %d, %v", n, got, err)
				}
			}
			if c.most > timedPiece {
				t.Errorf("%d bytes went out under one deadline, more than a piece of %d", c.most, timedPiece)
			}
			if c.arms != tc.arms {
				t.Errorf("the deadline was set %d times, want %d", c.arms, tc.arms)
			}
		})
	}
}

// A deadline runs while nothing is being written, and between an event's
// frame and its payload something is: EventData reads the payload through for
// line breaks, which over 32 MiB took longer than the 200 ms timeout of the
// stall tests in transport/, so a deadline set with the frame had passed
// before the payload's first byte. A write too large for what is left of the
// piece therefore gets its own deadline, set immediately before it, rather
// than filling the piece under the old one.
func TestTimedWriterSetsADeadlineImmediatelyBeforeALargeWrite(t *testing.T) {
	var log stringLog
	w := NewTimedWriter(&log, log.arm, nil)
	w.Arm()
	_, _ = w.WriteString("event: next\ndata: ")
	if n, err := w.Write(make([]byte, timedPiece)); n != timedPiece || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if want := []string{"arm", "string", "arm", "bytes"}; !slices.Equal(log.ops, want) {
		t.Errorf("ops = %v, want %v", log.ops, want)
	}
}

// A buffering writer still holds the end of the piece just written. Setting
// the next deadline first would spend it on those bytes. The flush has one of
// its own before it, so each piece boundary is arm, flush, arm.
func TestTimedWriterFlushesAPieceBeforeTheNextDeadline(t *testing.T) {
	var log timedLog
	w := NewTimedWriter(&log, log.arm, log.flush)
	w.Arm()
	payload := bytes.Repeat([]byte("x"), 2*timedPiece+1)
	if n, err := w.Write(payload); n != len(payload) || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	want := []string{"arm", "bytes", "arm", "flush", "arm", "bytes", "arm", "flush", "arm", "bytes"}
	if !slices.Equal(log.ops, want) {
		t.Errorf("ops = %v, want %v", log.ops, want)
	}
	if !bytes.Equal(log.data.Bytes(), payload) {
		t.Error("the payload did not arrive as written")
	}
}

// A flush runs under a deadline set immediately before it. The frame of an
// event is written under the deadline set with it, and EventData then reads
// the payload through for line breaks before passing any of it on, which over
// 32 MiB takes about a second under -race. A buffering writer's flush of the
// frame, when the payload's first write arrives, ran under a deadline that
// had passed in the meantime. On fasthttp the status line goes out with the
// first body bytes, so the connection was closed before a client had read
// one: "status line = \"\", EOF" in the stall tests of transport/, on
// GitHub's runners and rarely anywhere faster, because whether the deadline
// is re-set before fasthttp's goroutine writes is a race.
func TestTimedWriterFlushesUnderADeadlineSetJustBeforeIt(t *testing.T) {
	var log timedLog
	w := NewTimedWriter(&log, log.arm, log.flush)
	w.Arm()
	_, _ = w.Write([]byte("event: next\ndata: "))
	if n, err := w.Write(make([]byte, timedPiece)); n != timedPiece || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	flushes := 0
	for i, op := range log.ops {
		if op != "flush" {
			continue
		}
		flushes++
		if i == 0 || log.ops[i-1] != "arm" {
			t.Errorf("ops = %v: the flush at %d does not follow a deadline", log.ops, i)
		}
	}
	if flushes == 0 {
		t.Errorf("ops = %v: nothing was flushed, so nothing was checked", log.ops)
	}
}

func TestTimedWriterWriteString(t *testing.T) {
	t.Run("a string too long for what is left of the piece goes through Write", func(t *testing.T) {
		var log stringLog
		w := NewTimedWriter(&log, log.arm, nil)
		w.Arm()
		_, _ = w.Write(make([]byte, timedPiece-2))
		if n, err := w.WriteString("abc"); n != 3 || err != nil {
			t.Fatalf("WriteString = %d, %v", n, err)
		}
		if want := []string{"arm", "bytes", "arm", "bytes"}; !slices.Equal(log.ops, want) {
			t.Errorf("ops = %v, want %v", log.ops, want)
		}
		if got := log.data.String(); !strings.HasSuffix(got, "abc") {
			t.Errorf("the stream ends %q, want abc", got[len(got)-3:])
		}
	})

	t.Run("a writer with no WriteString is given bytes", func(t *testing.T) {
		var log timedLog
		w := NewTimedWriter(&log, log.arm, nil)
		w.Arm()
		if n, err := w.WriteString(":\n\n"); n != 3 || err != nil {
			t.Fatalf("WriteString = %d, %v", n, err)
		}
		if got := log.data.String(); got != ":\n\n" {
			t.Errorf("stream = %q", got)
		}
	})

	t.Run("a string that fits is not copied", func(t *testing.T) {
		w := NewTimedWriter(io.Discard, func() {}, nil)
		// io.WriteString is how the handlers reach it: through an io.Writer.
		var out io.Writer = w
		allocs := testing.AllocsPerRun(100, func() {
			w.Arm()
			_, _ = io.WriteString(out, "event: complete\ndata:\n\n")
		})
		if allocs != 0 {
			t.Errorf("writing an event's frame allocated %.0f times, want none", allocs)
		}
	})
}

// What a failed write reports is what the transports go by: WriteBody tells a
// response that would not serialize from a client that left by whether
// anything was written.
func TestTimedWriterReportsWhatWasWrittenBeforeAFailure(t *testing.T) {
	log := timedLog{failAt: timedPiece + 1}
	w := NewTimedWriter(&log, log.arm, log.flush)
	w.Arm()
	n, err := w.Write(make([]byte, 3*timedPiece))
	if n != timedPiece || !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Write = %d, %v; want one piece of %d and the writer's error", n, err, timedPiece)
	}

	flushFails := errors.New("flush failed")
	var sink bytes.Buffer
	w = NewTimedWriter(&sink, func() {}, func() error { return flushFails })
	w.Arm()
	n, err = w.Write(make([]byte, 3*timedPiece))
	if n != timedPiece || !errors.Is(err, flushFails) {
		t.Errorf("Write = %d, %v; want one piece of %d and the flush's error", n, err, timedPiece)
	}
}
