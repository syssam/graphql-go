package jsonw

import (
	"encoding/json"
	"testing"
)

// FuzzString checks that every string is escaped into valid JSON that decodes
// back to the original value.
func FuzzString(f *testing.F) {
	for _, seed := range []string{"", "plain", "quote\"back\\slash", "\x00\x1f\u2028\u2029", "emoji 🎉", "\xff\xfe invalid utf8", "</script>"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w := New()
		w.String(s)
		var decoded string
		if err := json.Unmarshal(w.Bytes(), &decoded); err != nil {
			t.Fatalf("invalid JSON %q for %q: %v", w.Bytes(), s, err)
		}
		// Invalid UTF-8 is replaced, as encoding/json does, so compare through
		// the standard library.
		want, _ := json.Marshal(s)
		var wantDecoded string
		_ = json.Unmarshal(want, &wantDecoded)
		if decoded != wantDecoded {
			t.Fatalf("round trip mismatch: got %q want %q", decoded, wantDecoded)
		}
	})
}

// FuzzKey checks that keys, which take the fast path when no escaping is
// needed, are always encoded as valid object keys.
func FuzzKey(f *testing.F) {
	f.Add("id")
	f.Add("with\"quote")
	f.Add("")
	f.Fuzz(func(t *testing.T, key string) {
		w := New()
		w.BeginObject()
		w.Key(EncodeKey(key))
		w.Null()
		w.EndObject()
		var m map[string]any
		if err := json.Unmarshal(w.Bytes(), &m); err != nil {
			t.Fatalf("invalid JSON %q: %v", w.Bytes(), err)
		}
	})
}
