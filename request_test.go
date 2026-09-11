package graphql

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/syssam/graphql-go/internal/jsonw"
)

func TestResponseWriteTo(t *testing.T) {
	cases := []struct {
		name string
		resp Response
		want string
	}{
		{"data only", Response{Data: []byte(`{"a":1}`)}, `{"data":{"a":1}}`},
		{"errors only", Response{Errors: []*Error{Errorf("e")}}, `{"errors":[{"message":"e"}]}`},
		{"both", Response{Data: []byte(`null`), Errors: []*Error{Errorf("e")}}, `{"errors":[{"message":"e"}],"data":null}`},
		{"extensions", Response{Data: []byte(`{}`), Extensions: map[string]any{"t": 1}}, `{"data":{},"extensions":{"t":1}}`},
		{"empty", Response{}, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := tc.resp.WriteTo(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if int(n) != buf.Len() {
				t.Fatalf("n = %d, want %d", n, buf.Len())
			}
			if !json.Valid(buf.Bytes()) {
				t.Fatal("invalid JSON")
			}
		})
	}
}

func TestResponseHasRequestErrors(t *testing.T) {
	if (&Response{Errors: []*Error{Errorf("e")}}).HasRequestErrors() != true {
		t.Fatal("errors without data must be request errors")
	}
	if (&Response{Data: []byte("null"), Errors: []*Error{Errorf("e")}}).HasRequestErrors() {
		t.Fatal("errors with data are field errors")
	}
}

func TestResponseReleaseIdempotent(t *testing.T) {
	w := jsonw.Get()
	w.Null()
	r := &Response{Data: w.Bytes(), buf: w}
	r.Release()
	if r.Data != nil || r.buf != nil {
		t.Fatal("Release must clear Data and buffer")
	}
	r.Release()
}

func TestRequestJSON(t *testing.T) {
	var req Request
	err := json.Unmarshal([]byte(`{"query":"{a}","operationName":"Q","variables":{"x":1},"extensions":{"p":true}}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Query != "{a}" || req.OperationName != "Q" || string(req.Variables) != `{"x":1}` || req.Extensions["p"] != true {
		t.Fatalf("unexpected: %+v", req)
	}
}
