package graphql

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

func TestSharedGoTypeOnTwoObjects(t *testing.T) {
	const sdl = `
		type User { id: ID! }
		type UserSummary { id: ID! }
		union Search = User | UserSummary
		type Query { user: User summary: UserSummary search: Search }
	`
	type person struct{ ID string }
	s, err := NewSchema(SDL(sdl),
		Object[person]("User", Field("id", func(p *person) string { return p.ID })),
		Object[person]("UserSummary", Field("id", func(p *person) string { return p.ID })),
		Query(
			Field("user", func(Root) *person { return &person{ID: "1"} }),
			Field("summary", func(Root) *person { return &person{ID: "2"} }),
			Field("search", func(Root) *person { return &person{ID: "1"} }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.goTypes[s.objects["User"].shapes.ptr]); n != 2 {
		t.Fatalf("shared Go type should list both objects, got %d", n)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ user { id } summary { id } }`, "")
	expectData(t, resp, `{"user":{"id":"1"},"summary":{"id":"2"}}`)

	resp = run(t, e, `{ search { __typename ... on User { id } ... on UserSummary { id } } }`, "")
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "TypeResolver") {
		t.Fatalf("ambiguous abstract must require TypeResolver, got %s", errorsJSON(resp.Errors))
	}

	s, err = NewSchema(SDL(sdl),
		Object[person]("User", Field("id", func(p *person) string { return p.ID })),
		Object[person]("UserSummary", Field("id", func(p *person) string { return p.ID })),
		Union[any]("Search", TypeResolver(func(v any) string { return "User" })),
		Query(
			Field("user", func(Root) *person { return &person{ID: "1"} }),
			Field("summary", func(Root) *person { return &person{ID: "2"} }),
			Field("search", func(Root) *person { return &person{ID: "1"} }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp = run(t, NewExecutor(s), `{ search { __typename ... on User { id } } }`, "")
	expectData(t, resp, `{"search":{"__typename":"User","id":"1"}}`)
}

func TestLiteralAndVariableNumbersMatch(t *testing.T) {
	const sdl = `scalar Big type Query { echo(n: Big!): String! }`
	var seen []any
	s, err := NewSchema(SDL(sdl),
		Scalar("Big", func(w *Writer, v json.Number) error {
			w.String(v.String())
			return nil
		}, func(raw any) (json.Number, error) {
			seen = append(seen, raw)
			n, ok := raw.(json.Number)
			if !ok {
				return "", Errorf("Big expected json.Number, got %T", raw)
			}
			return n, nil
		}),
		Args[struct{ N json.Number }](InputField("n", func(a *struct{ N json.Number }, v json.Number) { a.N = v })),
		Query(FieldArgs("echo", func(_ Root, a struct{ N json.Number }) string { return string(a.N) })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ echo(n: 42) }`, "")
	expectData(t, resp, `{"echo":"42"}`)
	resp = run(t, e, `query($n: Big!) { echo(n: $n) }`, `{"n":42}`)
	expectData(t, resp, `{"echo":"42"}`)
	if len(seen) == 0 {
		t.Fatal("scalar unmarshaler was never called")
	}
	for i, v := range seen {
		if _, ok := v.(json.Number); !ok {
			t.Fatalf("input %d: %T %v, want json.Number", i, v, v)
		}
	}
}

const objectDirSDL = `
directive @audit on OBJECT
type Item @audit { name: String! }
type Query { item: Item! }
`

func TestObjectDirectiveWrapsFields(t *testing.T) {
	var hits int
	s, err := NewSchema(SDL(objectDirSDL),
		Directive("audit", func(next FieldFunc) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				hits++
				return next(ctx, parent, args)
			}
		}),
		Object[dItem]("Item", Field("name", func(i *dItem) string { return i.Name })),
		Query(Field("item", func(Root) *dItem { return &dItem{Name: "x"} })),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := run(t, NewExecutor(s), `{ item { name } }`, "")
	expectData(t, resp, `{"item":{"name":"x"}}`)
	if hits != 1 {
		t.Fatalf("object directive should wrap Item.name once, hits=%d", hits)
	}
}

func TestQueryConstructorUsesSchemaRootName(t *testing.T) {
	s, err := NewSchema(SDL(`schema { query: RootQuery } type RootQuery { ok: Boolean! }`),
		Query(Field("ok", func(Root) bool { return true })),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.query == nil || s.query.name != "RootQuery" {
		t.Fatalf("query root = %+v", s.query)
	}
	resp := run(t, NewExecutor(s), `{ ok }`, "")
	expectData(t, resp, `{"ok":true}`)
}

// TestWriterEmitsEveryJSONKind covers the Writer handed to scalar marshalers.
// Only String was exercised anywhere; the rest of the type had no test at all.
func TestWriterEmitsEveryJSONKind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*Writer) error
		want  string
	}{
		{"string", func(w *Writer) error { w.String("a\"b"); return nil }, `"a\"b"`},
		{"int64", func(w *Writer) error { w.Int64(-42); return nil }, `-42`},
		{"uint64", func(w *Writer) error { w.Uint64(42); return nil }, `42`},
		{"float64", func(w *Writer) error { return w.Float64(1.5) }, `1.5`},
		{"bool", func(w *Writer) error { w.Bool(true); return nil }, `true`},
		{"null", func(w *Writer) error { w.Null(); return nil }, `null`},
		{"raw", func(w *Writer) error { w.Raw([]byte(`{"a":1}`)); return nil }, `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := jsonw.Get()
			defer jsonw.Put(inner)
			w := (*Writer)(inner)
			if err := tc.write(w); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := string(inner.Bytes()); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestWriterFloat64RejectsNonFinite pins the documented behaviour: JSON has no
// NaN or infinity, so a scalar marshaler must be told rather than emitting
// something unparseable.
func TestWriterFloat64RejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		inner := jsonw.Get()
		w := (*Writer)(inner)
		if err := w.Float64(v); err == nil {
			t.Errorf("Float64(%v) = nil error, want a rejection", v)
		}
		jsonw.Put(inner)
	}
}

// TestChainInterceptors covers the two Chain helpers, which had no test:
// interceptors must run outermost first and each must see the next one.
func TestChainInterceptors(t *testing.T) {
	var order []string

	reqA := RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
		order = append(order, "reqA")
		return next(ctx, req)
	})
	reqB := RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
		order = append(order, "reqB")
		return next(ctx, req)
	})
	opA := OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
		order = append(order, "opA")
		return next(ctx, oc)
	})
	opB := OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
		order = append(order, "opB")
		return next(ctx, oc)
	})

	s, err := NewSchema(SDL(`type Query { ping: String! }`),
		Query(Resolve("ping", func(context.Context, Root) (string, error) { return "pong", nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s,
		WithRequestInterceptor(ChainRequestInterceptors(reqA, reqB)),
		WithOperationInterceptor(ChainOperationInterceptors(opA, opB)),
		WithPlanCache(8),
	)
	expectData(t, run(t, e, `{ ping }`, ""), `{"ping":"pong"}`)

	want := "reqA,reqB,opA,opB"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("interceptor order = %s, want %s", got, want)
	}
}

// TestOperationContextStatsAndSelection covers Depth, Cost, Selection.IsEmpty
// and Selection.Collect, none of which had a test.
func TestOperationContextStatsAndSelection(t *testing.T) {
	var depth, cost int
	var leafEmpty bool
	var names []string

	s, err := NewSchema(SDL(`
		type Inner { a: String! b: String! }
		type Query { inner: Inner! }
	`),
		Object[struct{}]("Inner",
			Field("a", func(*struct{}) string { return "A" }),
			Field("b", func(*struct{}) string { return "B" }),
		),
		Query(Resolve("inner", func(ctx context.Context, _ Root) (*struct{}, error) {
			sel := SelectionFrom(ctx)
			leafEmpty = sel.IsEmpty()
			names = sel.Collect(func(f SelectedField) string { return f.Name })
			return &struct{}{}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			resp := next(ctx, oc)
			depth, cost = oc.Depth(), oc.Cost()
			return resp
		})))

	expectData(t, run(t, e, `{ inner { a b } }`, ""), `{"inner":{"a":"A","b":"B"}}`)

	if leafEmpty {
		t.Error("Selection under inner should not be empty")
	}
	if got := strings.Join(names, ","); got != "a,b" {
		t.Fatalf("Collect = %q, want \"a,b\"", got)
	}
	if depth != 2 {
		t.Errorf("Depth = %d, want 2", depth)
	}
	if cost <= 0 {
		t.Errorf("Cost = %d, want a positive default cost", cost)
	}
}

// TestExecutorSchemaAndOperationKind covers two exported accessors that
// transports rely on to route a request before executing it.
func TestExecutorSchemaAndOperationKind(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: String! } type Mutation { b: String! }`),
		Query(Resolve("a", func(context.Context, Root) (string, error) { return "a", nil })),
		Mutation(Resolve("b", func(context.Context, Root) (string, error) { return "b", nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)

	if e.Schema() != s {
		t.Error("Schema() did not return the schema the executor was built with")
	}
	for _, tc := range []struct {
		query string
		want  ast.Operation
	}{
		{`{ a }`, ast.Query},
		{`mutation { b }`, ast.Mutation},
	} {
		got, err := e.OperationKind(tc.query, "")
		if err != nil {
			t.Fatalf("OperationKind(%q): %v", tc.query, err)
		}
		if got != tc.want {
			t.Errorf("OperationKind(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
	if _, err := e.OperationKind(`{ this is not graphql`, ""); err == nil {
		t.Error("OperationKind on an unparseable document must fail")
	}
}
