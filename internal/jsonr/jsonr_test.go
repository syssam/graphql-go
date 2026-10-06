package jsonr

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// reference is what Object replaced: encoding/json with UseNumber, into
// map[string]any. It also refuses anything after the value, which
// json.Decoder on its own does not.
func reference(data []byte) (map[string]any, error) {
	// JSON's whitespace and no more: strings.TrimSpace also takes \v, \f and
	// the Unicode spaces, none of which JSON allows between tokens.
	if strings.Trim(string(data), " \t\n\r") == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the value")
	}
	return m, nil
}

// agree fails when the two decoders disagree on whether input is acceptable
// or on what it decodes to.
func agree(t *testing.T, input string) {
	t.Helper()
	want, wantErr := reference([]byte(input))
	got, gotErr := Object([]byte(input))
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("%q: encoding/json says err=%v, Object says err=%v", input, wantErr, gotErr)
	}
	if wantErr == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("%q:\n  encoding/json: %#v\n  Object:        %#v", input, want, got)
	}
}

var corpus = []string{
	// What requests send.
	``, ` `, "\n\t\r ", `null`, ` null `, `{}`, ` { } `,
	`{"f":10,"n":"ada"}`,
	`{"input":{"name":"Widget","priceCents":1299,"tags":["a","b","c"],"active":true,"note":null},"first":20,"after":"Y3Vyc29yOjEw"}`,
	`{"a":[],"b":{},"c":[[]],"d":[{}],"e":[null,true,false]}`,
	`{"ids":["1","2"],"where":{"and":[{"x":{"gt":1}},{"y":{"in":[1,2,3]}}]}}`,

	// Numbers keep their text.
	`{"n":0}`, `{"n":-0}`, `{"n":1.5}`, `{"n":-1.5e10}`, `{"n":1E+2}`, `{"n":1e-2}`,
	`{"n":9007199254740993}`, `{"n":123456789012345678901234567890}`, `{"n":1.0}`,
	`{"n":01}`, `{"n":1.}`, `{"n":.5}`, `{"n":-}`, `{"n":1e}`, `{"n":1e+}`, `{"n":+1}`,
	`{"n":0x10}`, `{"n":1_000}`, `{"n":NaN}`, `{"n":Infinity}`, `{"n":--1}`, `{"n":1.2.3}`,

	// Strings.
	`{"s":""}`, `{"":1}`, `{"s":"a\"b"}`, `{"s":"a\\b"}`, `{"s":"a\/b"}`,
	`{"s":"\b\f\n\r\t"}`, `{"s":"\u0041"}`, `{"s":"\u00e9"}`, `{"s":"\u0000"}`,
	`{"s":"\ud83d\ude00"}`, `{"s":"\uD83D\uDE00"}`, `{"s":"\ud83d"}`, `{"s":"\ude00"}`,
	`{"s":"\ud83dx"}`, `{"s":"\ud83d\u0041"}`, `{"s":"\ud83d\ud83d\ude00"}`, `{"s":"\ud83d\\"}`,
	`{"s":"é"}`, `{"s":"日本語"}`, `{"s":"😀"}`, `{"é":"é"}`,
	`{"s":"\x"}`, `{"s":"\u12"}`, `{"s":"\u12g4"}`, `{"s":"\`, `{"s":"abc`, `{"s":"a` + "\n" + `b"}`,
	`{"s":"a` + "\x00" + `b"}`, `{"s":"a` + "\x1f" + `b"}`, `{"s":"a` + "\x7f" + `b"}`,
	// A raw control character after an escape: the string is built rather than
	// sliced from there on, and that path refuses one by a check of its own.
	`{"s":"\n` + "\x01" + `"}`, `{"s":"\\` + "\x1f" + `b"}`, `{"s":"a\u0041` + "\x00" + `"}`, `{"` + `\b` + "\x01" + `":1}`,
	`{"s":"\n` + "\x7f" + `"}`,
	// Not valid UTF-8: repaired byte for byte, not refused.
	"{\"s\":\"\xff\"}", "{\"s\":\"a\xffb\"}", "{\"s\":\"\xe2\x82\"}", "{\"s\":\"\xe2\x82\xac\"}",
	"{\"s\":\"\xc0\x80\"}", "{\"s\":\"\xed\xa0\x80\"}", "{\"\xff\":1}", "{\"s\":\"\\n\xff\"}",
	"{\"s\":\"\xff\\n\"}", "{\"s\":\"\xf0\x9f\x98\"}",

	// Structure.
	`{"a":1,"a":2}`, `{"a":{"x":1},"a":{"y":2}}`, `{"a":1,}`, `{,}`, `{"a"}`, `{"a":}`, `{"a" 1}`,
	`{"a":1 "b":2}`, `{a:1}`, `{'a':1}`, `{"a":1`, `{`, `}`, `[`, `{"a":[1,]}`, `{"a":[1 2]}`,
	`{"a":[`, `{"a":[1`, `{"a":{`, `{"a":tru}`, `{"a":t}`, `{"a":True}`, `{"a":nul}`, `{"a":NULL}`,
	`{"a":falsey}`, `{"a":truee}`, `{"a":nullx}`, `{"a":1}x`, `{"a":1} {}`, `{"a":1}}`, `{}{}`, `nullx`, `nul`,
	`{"a":1} ` + "\n", `[]`, `[1]`, `"x"`, `1`, `true`, `false`,
	// Whitespace JSON does not count as whitespace.
	"{\"a\":\v1}", "{\"a\":\f1}", "\u00a0{}", "\ufeff{}", "{}\ufeff",
	// Comments are not JSON.
	`{"a":1 /* c */}`, `{"a":1} // c`,
}

func TestObjectAgreesWithEncodingJSON(t *testing.T) {
	// Each input a second time with a member long enough to pass shareLimit,
	// where strings are copied out instead of shared: the same values either
	// way, and a fuzzer rarely grows an input that far on its own.
	pad := `{"_pad":"` + strings.Repeat("p", shareLimit) + `",`
	for _, input := range corpus {
		agree(t, input)
		agree(t, strings.Replace(input, "{", pad, 1))
	}
}

// Depth is bounded as encoding/json bounds it, since the decoder recurses.
func TestObjectBoundsNesting(t *testing.T) {
	for _, n := range []int{maxDepth - 2, maxDepth - 1, maxDepth, maxDepth + 1} {
		agree(t, `{"a":`+strings.Repeat("[", n)+strings.Repeat("]", n)+`}`)
		agree(t, strings.Repeat(`{"a":`, n)+`1`+strings.Repeat("}", n))
	}
}

// The result must not alias the caller's buffer: a transport over fasthttp
// hands in memory another request overwrites as soon as this one returns.
func TestObjectDoesNotAliasItsInput(t *testing.T) {
	data := []byte(`{"name":"ada","n":12,"list":["x"]}`)
	m, err := Object(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = '#'
	}
	want := map[string]any{"name": "ada", "n": json.Number("12"), "list": []any{"x"}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("decoded values changed when the input buffer was overwritten: %#v", m)
	}
}

// Strings are substrings of one copy of the input, which is what makes a
// small payload cheap -- and would make one short string keep the whole of a
// large payload reachable for as long as a resolver holds on to it: an id put
// in a cache, beside an upload in the same variables. Past shareLimit every
// string is its own allocation, as it was with encoding/json, so what a kept
// value can pin is bounded by that limit and not by what a client sends.
func TestObjectDoesNotPinALargePayload(t *testing.T) {
	const payloads, size = 8, 4 << 20
	big := strings.Repeat("x", size)
	live := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := live()
	kept := make([]string, 0, payloads)
	keys := make([]string, 0, payloads)
	numbers := make([]json.Number, 0, payloads)
	for range payloads {
		m, err := Object([]byte(`{"id":"abc","n":12,"upload":"` + big + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, m["id"].(string))
		numbers = append(numbers, m["n"].(json.Number))
		for k := range m {
			if k == "id" {
				keys = append(keys, k)
			}
		}
	}
	after := live()
	// With the payloads pinned this is 32 MiB; without, a few hundred bytes
	// and whatever else the runtime did meanwhile.
	if grew := int64(after) - int64(before); grew > size {
		t.Errorf("holding %d short values keeps %d bytes alive, so each one pins the payload it came from", payloads, grew)
	}
	runtime.KeepAlive(kept)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(numbers)
}

// Below the limit nothing is copied twice: that is the whole saving.
func TestObjectSharesASmallPayload(t *testing.T) {
	data := []byte(`{"first":20,"after":"Y3Vyc29yOjEw","name":"ada"}`)
	if len(data) > shareLimit {
		t.Fatalf("the fixture is %d bytes, past shareLimit", len(data))
	}
	// The copy of the input, the map, and one for each of the three values
	// as it goes into an interface. Copied one by one it is twelve.
	if n := testing.AllocsPerRun(100, func() {
		if _, err := Object(data); err != nil {
			t.Fatal(err)
		}
	}); n > 6 {
		t.Errorf("a small object took %v allocations; its strings are being copied one by one", n)
	}
}

func FuzzObject(f *testing.F) {
	for _, input := range corpus {
		f.Add([]byte(input))
	}
	f.Add([]byte(`{"_pad":"` + strings.Repeat("p", shareLimit) + `","s":"a\u00e9","n":-1.5e10,"l":["x",1]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		want, wantErr := reference(data)
		got, gotErr := Object(data)
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("%q: encoding/json says err=%v, Object says err=%v", data, wantErr, gotErr)
		}
		if wantErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("%q:\n  encoding/json: %#v\n  Object:        %#v", data, want, got)
		}
	})
}

func BenchmarkObject(b *testing.B) {
	for name, raw := range map[string][]byte{
		"two scalars":  []byte(`{"f":10,"n":"ada"}`),
		"input object": []byte(`{"input":{"name":"Widget","priceCents":1299,"tags":["a","b","c"],"active":true,"note":null},"first":20,"after":"Y3Vyc29yOjEw"}`),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Object(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
