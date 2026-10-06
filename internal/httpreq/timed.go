package httpreq

import "io"

// timedPiece is how much of a stream goes out under one write deadline. A
// deadline over a whole payload would bound its size by the client's
// bandwidth: the largest response is 64 MiB by default, and at ten seconds
// that refuses any client slower than 6 MiB a second. A piece at a time, it
// asks only that the client is still taking something.
const timedPiece = 32 << 10

// TimedWriter writes a Server-Sent Events stream under a write timeout it
// does not own: arm is the transport's way of giving the bytes about to be
// written a deadline -- an http.ResponseController's on net/http, the
// connection's on fasthttp -- and this type decides when to call it.
//
// That is once per event, from Arm, and again before a write too large for
// what is left of the piece. An event is several writes, the frame around a
// payload and the parts of the payload, and setting the deadline before each
// of them was five calls where one bounds the same bytes. What the deadline
// promises is unchanged: no more than a piece is ever written under one.
//
// A write that does not fit is given a deadline of its own rather than
// filling the piece first, because a deadline also runs while nothing is being
// written. EventData reads a payload through for line breaks before passing
// it on, and over tens of megabytes that outlasts a short timeout: set once
// with the frame, the deadline had passed before the first byte of the payload
// was written. Under this rule what happens between a deadline and a write it
// covers is work on less than a piece.
//
// flush, when there is one, runs before the deadline is set again. A writer
// that buffers -- fasthttp's stream writer -- would otherwise have the new
// deadline cover bytes of the piece before, still on their way out.
type TimedWriter struct {
	w io.Writer
	// sw is w when it takes a string as it is, which saves the frame around
	// every event a copy into a fresh slice.
	sw    io.StringWriter
	arm   func()
	flush func() error
	// sent counts what has been written since the deadline was last set.
	sent int
}

// NewTimedWriter returns a TimedWriter over w. flush may be nil.
func NewTimedWriter(w io.Writer, arm func(), flush func() error) *TimedWriter {
	sw, _ := w.(io.StringWriter)
	return &TimedWriter{w: w, sw: sw, arm: arm, flush: flush}
}

// Arm sets the deadline for an event about to be written. The transport's
// flush of that event is covered by it, or by the last one Write set.
func (t *TimedWriter) Arm() {
	t.arm()
	t.sent = 0
}

func (t *TimedWriter) Write(p []byte) (int, error) {
	written := 0
	for {
		piece := p[written:min(len(p), written+timedPiece)]
		if t.sent+len(piece) > timedPiece {
			if t.flush != nil {
				if err := t.flush(); err != nil {
					return written, err
				}
			}
			t.Arm()
		}
		n, err := t.w.Write(piece)
		written += n
		t.sent += n
		if err != nil || written == len(p) {
			return written, err
		}
	}
}

// WriteString is Write without the copy, for a string that fits in what is
// left of the piece: an event's frame nearly always does. One that does not
// goes through Write.
func (t *TimedWriter) WriteString(s string) (int, error) {
	if t.sw == nil || len(s) > timedPiece-t.sent {
		return t.Write([]byte(s))
	}
	n, err := t.sw.WriteString(s)
	t.sent += n
	return n, err
}
