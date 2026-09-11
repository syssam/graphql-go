package jsonw

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func TestEmptyObject(t *testing.T) {
	w := New()
	w.BeginObject()
	w.EndObject()
	if got := string(w.Bytes()); got != "{}" {
		t.Fatalf("got %s, want {}", got)
	}
}

func TestObjectSeparators(t *testing.T) {
	w := New()
	w.BeginObject()
	w.Key(EncodeKey("a"))
	w.Int64(1)
	w.Key(EncodeKey("b"))
	w.BeginArray()
	w.BeginObject()
	w.KeyString("c")
	w.Bool(true)
	w.EndObject()
	w.Null()
	w.String("x")
	w.EndArray()
	w.EndObject()
	const want = `{"a":1,"b":[{"c":true},null,"x"]}`
	if got := string(w.Bytes()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if !json.Valid(w.Bytes()) {
		t.Fatal("output is not valid JSON")
	}
}

func TestRewindRestoresSeparatorState(t *testing.T) {
	direct := New()
	direct.BeginObject()
	direct.Key(EncodeKey("a"))
	direct.Int64(1)
	direct.Key(EncodeKey("c"))
	direct.Int64(3)
	direct.EndObject()

	w := New()
	w.BeginObject()
	w.Key(EncodeKey("a"))
	w.Int64(1)
	m := w.Mark()
	w.Key(EncodeKey("b"))
	w.BeginArray()
	w.Int64(2)
	w.Rewind(m)
	w.Key(EncodeKey("c"))
	w.Int64(3)
	w.EndObject()

	if got, want := string(w.Bytes()), string(direct.Bytes()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRewindAfterKeyThenNull(t *testing.T) {
	w := New()
	w.BeginObject()
	w.Key(EncodeKey("a"))
	m := w.Mark()
	w.BeginObject()
	w.Key(EncodeKey("x"))
	w.Int64(1)
	w.Rewind(m)
	w.Null()
	w.Key(EncodeKey("b"))
	w.Int64(2)
	w.EndObject()
	if got, want := string(w.Bytes()), `{"a":null,"b":2}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRewindArrayElement(t *testing.T) {
	w := New()
	w.BeginArray()
	w.Int64(1)
	m := w.Mark()
	w.BeginObject()
	w.Key(EncodeKey("x"))
	w.Rewind(m)
	w.Null()
	w.Int64(3)
	w.EndArray()
	if got, want := string(w.Bytes()), `[1,null,3]`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestFloat64(t *testing.T) {
	w := New()
	if err := w.Float64(math.NaN()); err != ErrNonFinite {
		t.Fatalf("NaN: err = %v, want ErrNonFinite", err)
	}
	if err := w.Float64(math.Inf(1)); err != ErrNonFinite {
		t.Fatalf("Inf: err = %v, want ErrNonFinite", err)
	}
	if w.Len() != 0 {
		t.Fatalf("non-finite floats must write nothing, got %q", w.Bytes())
	}
	for _, v := range []float64{0, 1, -1.5, 1e20, 1e21, 1e-6, 1e-7, 123456789.125, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		w.Reset()
		if err := w.Float64(v); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(v)
		if got := string(w.Bytes()); got != string(want) {
			t.Errorf("Float64(%v) = %s, want %s", v, got, want)
		}
	}
}

func TestStringEscaping(t *testing.T) {
	cases := map[string]string{
		"plain":            `"plain"`,
		`a"b`:              `"a\"b"`,
		`back\slash`:       `"back\\slash"`,
		"\n\r\t\b\f":       `"\n\r\t\b\f"`,
		"\x01\x1f":         `"\u0001\u001f"`,
		"\u2028\u2029":     `"\u2028\u2029"`,
		"<&>":              `"<&>"`,
		"日本語":              `"日本語"`,
		"bad\xffbyte":      `"bad\ufffdbyte"`,
		"emoji \U0001F600": `"emoji 😀"`,
	}
	for in, want := range cases {
		if got := string(AppendString(nil, in)); got != want {
			t.Errorf("AppendString(%q) = %s, want %s", in, got, want)
		}
		// Every output must round-trip through encoding/json.
		var back string
		if err := json.Unmarshal(AppendString(nil, in), &back); err != nil {
			t.Errorf("AppendString(%q) produced invalid JSON: %v", in, err)
		}
	}
}

func TestEncodeKey(t *testing.T) {
	if got := string(EncodeKey(`na"me`)); got != `"na\"me":` {
		t.Fatalf("got %s", got)
	}
}

func TestPool(t *testing.T) {
	w := Get()
	w.BeginArray()
	w.Int64(1)
	w.EndArray()
	Put(w)
	w2 := Get()
	if w2.Len() != 0 || len(w2.stack) != 0 || w2.pendingKey {
		t.Fatal("pooled writer must be reset")
	}
	Put(w2)
}

func TestUint64(t *testing.T) {
	w := New()
	w.Uint64(math.MaxUint64)
	if got := string(w.Bytes()); got != strconv.FormatUint(math.MaxUint64, 10) {
		t.Fatalf("got %s", got)
	}
}

func TestWriteObjectAllocs(t *testing.T) {
	keys := make([][]byte, 100)
	for i := range keys {
		keys[i] = EncodeKey("field" + strconv.Itoa(i))
	}
	w := New()
	// Warm the buffer so growth does not count as an allocation.
	writeObject(w, keys)
	allocs := testing.AllocsPerRun(100, func() {
		w.Reset()
		writeObject(w, keys)
	})
	if allocs != 0 {
		t.Fatalf("allocs/op = %v, want 0", allocs)
	}
}

func writeObject(w *Writer, keys [][]byte) {
	w.BeginObject()
	for _, k := range keys {
		w.Key(k)
		w.String("value")
	}
	w.EndObject()
}

func BenchmarkWriteObject(b *testing.B) {
	keys := make([][]byte, 100)
	for i := range keys {
		keys[i] = EncodeKey("field" + strconv.Itoa(i))
	}
	w := New()
	b.ReportAllocs()
	for b.Loop() {
		w.Reset()
		writeObject(w, keys)
	}
}
