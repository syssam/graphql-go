package httpreq

import (
	"bytes"
	"testing"
)

func TestEventDataKeepsAPayloadInsideOneDataField(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:     `{"a":1}`,
		"{\"a\":1\n}": "{\"a\":1\ndata: }",
		"a\r\nb":      "a\ndata: b",
		"a\rb":        "a\ndata: b",
		"a\n\nb":      "a\ndata: \ndata: b",
		"\n":          "\ndata: ",
		"":            "",
		"trailing\n":  "trailing\ndata: ",
		"x\ny\nz":     "x\ndata: y\ndata: z",
	} {
		var buf bytes.Buffer
		n, err := EventData(&buf).Write([]byte(in))
		if err != nil || n != len(in) {
			t.Errorf("Write(%q) = %d, %v; want %d, nil", in, n, err, len(in))
		}
		if got := buf.String(); got != want {
			t.Errorf("EventData(%q) wrote %q, want %q", in, got, want)
		}
	}
}
