package transport_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
)

type rawMeta struct{}

// sseEventData reassembles the data of the first event named name the way an
// EventSource does: every data line of the event, joined by a newline.
func sseEventData(stream, name string) (string, bool) {
	for _, event := range strings.Split(stream, "\n\n") {
		var data []string
		match := false
		for _, line := range strings.Split(event, "\n") {
			switch {
			case line == "event: "+name:
				match = true
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "data:":
				data = append(data, "")
			}
		}
		if match {
			return strings.Join(data, "\n"), true
		}
	}
	return "", false
}

// A custom scalar writes its own bytes, and one built on json.Encoder ends
// them with a newline -- which is how gqlgen's MarshalMap and MarshalAny are
// written, the types the Marshaler adapters exist to take as they are. The
// SSE handlers framed a response as one data line on the assumption that an
// execution buffer never holds a newline, so the event broke off at it and
// the client was handed half a JSON document.
func TestSSEFramesAPayloadThatContainsANewline(t *testing.T) {
	servers := newEquivServersWithExecutor(t, func(t *testing.T) *graphql.Executor {
		t.Helper()
		s, err := graphql.NewSchema(graphql.SDL(`scalar JSON type Query { meta: JSON! }`),
			graphql.Scalar("JSON",
				func(w *graphql.Writer, _ rawMeta) error { w.Raw([]byte("{\"a\":1}\n")); return nil },
				func(any) (rawMeta, error) { return rawMeta{}, nil }),
			graphql.Query(graphql.Field("meta", func(graphql.Root) rawMeta { return rawMeta{} })),
		)
		if err != nil {
			t.Fatalf("NewSchema: %v", err)
		}
		return graphql.NewExecutor(s)
	})
	for _, name := range sseFamily {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, servers[name]+"/graphql", strings.NewReader(`{"query":"{ meta }"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			data, ok := sseEventData(string(raw), "next")
			if !ok {
				t.Fatalf("no next event in %q", raw)
			}
			var got struct {
				Data struct {
					Meta map[string]int `json:"meta"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(data), &got); err != nil || got.Data.Meta["a"] != 1 {
				t.Fatalf("next event data is not the response: %q (%v), from stream %q", data, err, raw)
			}
		})
	}
}
