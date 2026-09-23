// Package jsonw implements a streaming JSON writer that appends directly to a
// byte buffer, tracks value separators per nesting level, and supports
// rewinding to an earlier mark so that partially written values can be
// replaced with null without re-serialization.
package jsonw

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ErrNonFinite is returned by Float64 for NaN and infinities, which JSON
// cannot represent.
var ErrNonFinite = errors.New("jsonw: non-finite float")

// maxPooledCap bounds the capacity of buffers returned to the pool. Past it a
// writer is reset and dropped, so the next request rebuilds its buffer from
// 512 bytes.
//
// The number is 16 MiB because append overshoots and the cliff is therefore
// well below the cap: at 4 MiB a 3.62 MB response reached a 4.12 MB capacity
// and was dropped -- 38.4 MB/op against 16.6 for a response 17% smaller. That
// query is usually reachable unauthenticated and is polled by GraphiQL, Apollo
// Studio and codegen tools, so it is not an exotic path.
//
// "Pins memory" reads worse than it is, twice over. sync.Pool is emptied by
// the collector, so a large buffer survives at most a couple of GC cycles. And
// the pool can only hold buffers that were actually created: to have one per P
// at this size a process must have just served that many concurrent responses
// of that size, which cost the same memory whether or not they are pooled.
// Raising it defers a release; it does not raise a peak.
//
// It was 8 MiB, chosen as "introspection to roughly 8 000 types". That
// estimate came from type count, and type count does not predict the response:
// a real 5 516-type schema introspects to 6.56 MB, because its types are twice
// as wide as the synthetic ones the estimate was read off. 6.56 MB reaches a
// capacity of 8.05 MiB -- one growth step over the cap -- and measured on that
// schema the query cost 71.3 MB/op with no reuse at all between requests,
// against 31.5 MB/op once it fits. An 8 MiB cap pools a response up to
// 6.4 MB; 16 MiB pools one up to 15.7 MB, because the growth steps get closer
// to exact as they get larger.
const maxPooledCap = 16 << 20

// Writer appends JSON to an internal buffer.
//
// Every value-writing method emits the separator required by the enclosing
// container, so callers never write commas themselves. Key writes the
// separator and the key; the value written immediately after it must not
// emit another separator, which the writer tracks with a pending-key flag.
type Writer struct {
	buf        []byte
	stack      []bool // per open container: whether a value has been written
	pendingKey bool

	// budget is nil when unlimited, &own on a root writer, and the root's on a
	// sub-writer, so concurrently written buffers count against one limit. It
	// lives here rather than on the executor's per-request state because the
	// writer is pooled: growing it costs no allocation per request.
	budget   *budget
	reported int // bytes of buf already added to budget.used
	own      budget
}

// budget is a byte limit shared by a root writer and its sub-writers.
type budget struct {
	used     atomic.Int64
	limit    int64
	exceeded atomic.Bool
}

// Mark captures writer state so that Rewind can restore it.
type Mark struct {
	off        int
	depth      int
	comma      bool
	pendingKey bool
}

// New returns an empty writer.
func New() *Writer {
	return &Writer{buf: make([]byte, 0, 512), stack: make([]bool, 0, 16)}
}

var pool = sync.Pool{New: func() any { return New() }}

// Get returns a reset writer from the pool.
func Get() *Writer {
	return pool.Get().(*Writer)
}

// Put resets w and returns it to the pool. A buffer too large to pool is still
// reset, so a writer sharing a limit gives its bytes back either way.
func Put(w *Writer) {
	if w == nil {
		return
	}
	w.Reset()
	if cap(w.buf) > maxPooledCap {
		return
	}
	pool.Put(w)
}

// Reset clears the buffer and all container state. A writer sharing another's
// limit gives back the bytes it reported, so a buffer returned to the pool
// stops counting against the response it was part of.
func (w *Writer) Reset() {
	if w.budget != nil {
		if w.budget != &w.own {
			w.budget.used.Add(-int64(w.reported))
		}
		w.budget = nil
		w.reported = 0
		w.own.used.Store(0)
		w.own.limit = 0
		w.own.exceeded.Store(false)
	}
	w.buf = w.buf[:0]
	w.stack = w.stack[:0]
	w.pendingKey = false
}

// Bytes returns the written JSON. The slice aliases the internal buffer and
// is invalidated by further writes or Reset.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of bytes written so far.
func (w *Writer) Len() int { return len(w.buf) }

// Limit bounds the bytes held by w and every writer sharing its limit. It is
// called on a reset writer before anything is written.
func (w *Writer) Limit(n int64) {
	w.own.limit = n
	w.budget = &w.own
}

// ShareLimit makes w count against from's limit, if from has one. A sharing
// writer must be Reset before the root is, so the bytes it gives back leave
// the count they were added to.
func (w *Writer) ShareLimit(from *Writer) {
	w.budget = from.budget
}

// OverLimit reports this writer's growth since its last call and whether the
// shared limit has been passed. Once passed it stays passed, even if rewinds
// later shrink the count, so execution keeps stopping. With no limit it is a
// nil compare, kept small enough to inline.
func (w *Writer) OverLimit() bool {
	if w.budget == nil {
		return false
	}
	return w.overLimit()
}

// overLimit reports on every call rather than in fixed-size blocks: a block
// would only be reported once a writer grew a whole one, and a concurrent list
// element's buffer is usually far smaller, so thousands of them would never
// report at all.
//
// It is kept out of line because inlining it into OverLimit pushes OverLimit
// past the inliner's budget, and the unlimited path would then cost a call.
//
//go:noinline
func (w *Writer) overLimit() bool {
	b := w.budget
	if d := len(w.buf) - w.reported; d != 0 {
		b.used.Add(int64(d))
		w.reported = len(w.buf)
	}
	if b.exceeded.Load() {
		return true
	}
	if b.used.Load() > b.limit {
		b.exceeded.Store(true)
		return true
	}
	return false
}

// LimitExceeded reports whether any OverLimit call on this writer's limit
// found it passed.
func (w *Writer) LimitExceeded() bool {
	return w.budget != nil && w.budget.exceeded.Load()
}

// Mark records the current position and separator state.
func (w *Writer) Mark() Mark {
	m := Mark{off: len(w.buf), depth: len(w.stack), pendingKey: w.pendingKey}
	if m.depth > 0 {
		m.comma = w.stack[m.depth-1]
	}
	return m
}

// Rewind discards everything written after m and restores separator state.
func (w *Writer) Rewind(m Mark) {
	w.buf = w.buf[:m.off]
	w.stack = w.stack[:m.depth]
	if m.depth > 0 {
		w.stack[m.depth-1] = m.comma
	}
	w.pendingKey = m.pendingKey
}

// sep emits the comma required before a value in the current container,
// unless a key was just written.
func (w *Writer) sep() {
	if w.pendingKey {
		w.pendingKey = false
		return
	}
	if n := len(w.stack); n > 0 {
		if w.stack[n-1] {
			w.buf = append(w.buf, ',')
		}
		w.stack[n-1] = true
	}
}

// BeginObject writes '{' and opens a new container.
func (w *Writer) BeginObject() {
	w.sep()
	w.buf = append(w.buf, '{')
	w.stack = append(w.stack, false)
}

// EndObject writes '}' and closes the current container.
func (w *Writer) EndObject() {
	w.buf = append(w.buf, '}')
	w.stack = w.stack[:len(w.stack)-1]
}

// BeginArray writes '[' and opens a new container.
func (w *Writer) BeginArray() {
	w.sep()
	w.buf = append(w.buf, '[')
	w.stack = append(w.stack, false)
}

// EndArray writes ']' and closes the current container.
func (w *Writer) EndArray() {
	w.buf = append(w.buf, ']')
	w.stack = w.stack[:len(w.stack)-1]
}

// Key writes a pre-encoded object key. raw must already have the form
// `"name":` as produced by EncodeKey.
func (w *Writer) Key(raw []byte) {
	w.sep()
	w.buf = append(w.buf, raw...)
	w.pendingKey = true
}

// KeyString escapes name and writes it as an object key.
func (w *Writer) KeyString(name string) {
	w.sep()
	w.buf = AppendString(w.buf, name)
	w.buf = append(w.buf, ':')
	w.pendingKey = true
}

// Null writes null.
func (w *Writer) Null() {
	w.sep()
	w.buf = append(w.buf, "null"...)
}

// Bool writes true or false.
func (w *Writer) Bool(v bool) {
	w.sep()
	if v {
		w.buf = append(w.buf, "true"...)
	} else {
		w.buf = append(w.buf, "false"...)
	}
}

// Int64 writes a signed integer.
func (w *Writer) Int64(v int64) {
	w.sep()
	w.buf = strconv.AppendInt(w.buf, v, 10)
}

// Uint64 writes an unsigned integer.
func (w *Writer) Uint64(v uint64) {
	w.sep()
	w.buf = strconv.AppendUint(w.buf, v, 10)
}

// Float64 writes a finite float using the same formatting as encoding/json.
// It writes nothing and returns ErrNonFinite for NaN or infinities.
func (w *Writer) Float64(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ErrNonFinite
	}
	w.sep()
	w.buf = AppendFloat(w.buf, v)
	return nil
}

// String writes an escaped JSON string.
func (w *Writer) String(s string) {
	w.sep()
	w.buf = AppendString(w.buf, s)
}

// Raw writes an already-encoded JSON value verbatim.
func (w *Writer) Raw(b []byte) {
	w.sep()
	w.buf = append(w.buf, b...)
}

// Splice writes sub's bytes as the next value. The bytes sub already reported
// against a shared limit become w's, so they are counted once: sub is
// typically put back only after every sibling is spliced, and a checkpoint on
// w before then would otherwise count them a second time.
func (w *Writer) Splice(sub *Writer) {
	w.Raw(sub.buf)
	w.reported += sub.reported
	sub.reported = 0
}

// EncodeKey returns `"name":` with name escaped, suitable for Key.
func EncodeKey(name string) []byte {
	b := AppendString(make([]byte, 0, len(name)+3), name)
	return append(b, ':')
}

// AppendFloat appends v formatted like encoding/json does: fixed notation in
// the common range and exponent notation with a trimmed exponent otherwise.
func AppendFloat(dst []byte, v float64) []byte {
	abs := math.Abs(v)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, v, format, -1, 64)
	if format == 'e' {
		// Turn "1e-09" into "1e-9" to match encoding/json.
		n := len(dst)
		if n-start >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

const hexDigits = "0123456789abcdef"

// AppendString appends s as a quoted JSON string. It escapes quotes,
// backslashes, control characters and U+2028/U+2029, replaces invalid UTF-8
// with U+FFFD, and leaves '<', '>' and '&' untouched.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '"', '\\':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
