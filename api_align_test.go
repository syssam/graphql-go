package graphql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
