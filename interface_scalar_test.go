package graphql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type anyArgs struct {
	V any `graphql:"v"`
}

func anyEchoExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
scalar JSON
type Query { echo(v: JSON): JSON echoRequired(v: JSON!): JSON }
`),
		Scalar[any]("JSON",
			func(w *Writer, v any) error {
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				w.Raw(b)
				return nil
			},
			func(v any) (any, error) { return v, nil }),
		Args[anyArgs](),
		Query(
			ResolveArgs("echo", func(_ context.Context, _ Root, a anyArgs) (any, error) { return a.V, nil }),
			ResolveArgs("echoRequired", func(_ context.Context, _ Root, a anyArgs) (any, error) { return a.V, nil }),
		))
	if err != nil {
		t.Fatalf("a Go interface holds nil, so it can back a nullable position; the schema must build: %v", err)
	}
	return NewExecutor(s)
}

// An interface value is nilable, so `any` backs a nullable scalar argument without a pointer:
// absent and null arrive as nil, anything else as the decoded value.
func TestInterfaceTypedScalarBacksANullableArgument(t *testing.T) {
	exec := anyEchoExecutor(t)
	run := func(q string) *Response { return exec.Execute(context.Background(), &Request{Query: q}) }

	for q, want := range map[string]string{
		`{ echo }`:                 `{"echo":null}`,
		`{ echo(v: null) }`:        `{"echo":null}`,
		`{ echo(v: {a: 1}) }`:      `{"echo":{"a":1}}`,
		`{ echo(v: [1, "x"]) }`:    `{"echo":[1,"x"]}`,
		`{ echoRequired(v: 7) }`:   `{"echoRequired":7}`,
		`{ echoRequired(v: "s") }`: `{"echoRequired":"s"}`,
	} {
		r := run(q)
		if len(r.Errors) != 0 || string(r.Data) != want {
			t.Errorf("%s: data=%s errors=%v, want %s", q, r.Data, r.Errors, want)
		}
	}
	// a NON-NULL position still refuses null: that is the coercion's job, not the Go type's
	if r := run(`{ echoRequired(v: null) }`); len(r.Errors) == 0 || !strings.Contains(r.Errors[0].Message, "null") {
		t.Errorf("JSON! must still refuse null, got data=%s errors=%v", r.Data, r.Errors)
	}
}
