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

// The same holds on the way out: nil is null, so it is written as null and
// never handed to marshal -- as a nil *E never is -- and at a non-null
// position it is the error any other null is there. A marshal given nil wrote
// "null" itself, and JSON! answered null with no error.
func TestInterfaceTypedScalarNilIsNullOnOutput(t *testing.T) {
	marshalledNil := false
	s, err := NewSchema(SDL(`
scalar JSON
type Box { j: JSON! }
type Query { box: Box jn: JSON js: [JSON!] jl: [JSON] }
`),
		Scalar[any]("JSON",
			func(w *Writer, v any) error {
				marshalledNil = marshalledNil || v == nil
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				w.Raw(b)
				return nil
			},
			func(v any) (any, error) { return v, nil }),
		Object[struct{}]("Box", Field("j", func(*struct{}) any { return nil })),
		Query(
			Field("box", func(Root) *struct{} { return &struct{}{} }),
			Field("jn", func(Root) any { return nil }),
			Field("js", func(Root) []any { return []any{1, nil} }),
			Field("jl", func(Root) []any { return []any{1, nil} }),
		))
	if err != nil {
		t.Fatal(err)
	}
	resp := run(t, NewExecutor(s), `{ box { j } jn js jl }`, "")
	if got := string(resp.Data); got != `{"box":null,"jn":null,"js":null,"jl":[1,null]}` {
		t.Fatalf("data = %s (errors %s)", got, errorsJSON(resp.Errors))
	}
	if len(resp.Errors) != 2 {
		t.Fatalf("want an error for Box.j and one for js[1], got %s", errorsJSON(resp.Errors))
	}
	for i, path := range []string{"box.j", "js[1]"} {
		if e := resp.Errors[i]; e.Path.String() != path || !strings.Contains(e.Message, "Cannot return null for non-nullable") {
			t.Errorf("error %d = %q at %s, want the non-null violation at %s", i, e.Message, e.Path.String(), path)
		}
	}
	if marshalledNil {
		t.Error("marshal was called with nil")
	}
}
