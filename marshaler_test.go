package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// mField is shaped like the order fields ent and velox generate: a struct
// holding a func, so not comparable and not bindable by Enum[T comparable],
// whose values only UnmarshalGQL can construct. MarshalGQL writes in three
// calls, as velox's Cursor does, which is what a naive io.Writer adapter
// would separate with commas.
type mField struct {
	column string
	less   func(a, b int) bool
}

func (f mField) MarshalGQL(w io.Writer) {
	if f.column == "bogus" {
		io.WriteString(w, `"BOGUS"`)
		return
	}
	if f.column == "silent" {
		return
	}
	io.WriteString(w, `"`)
	io.WriteString(w, strings.ToUpper(f.column))
	io.WriteString(w, `"`)
}

func (f *mField) UnmarshalGQL(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("mField must be a string, got %T", v)
	}
	switch s {
	case "NAME", "PRICE":
		*f = mField{column: strings.ToLower(s), less: func(a, b int) bool { return a < b }}
		return nil
	}
	return fmt.Errorf("%s is not a valid mField", s)
}

// mCursor is a custom scalar with the same contract.
type mCursor struct{ id int }

func (c mCursor) MarshalGQL(w io.Writer) {
	io.WriteString(w, `"c:`)
	io.WriteString(w, strconv.Itoa(c.id))
	io.WriteString(w, `"`)
}

func (c *mCursor) UnmarshalGQL(v any) error {
	s, _ := v.(string)
	n, err := strconv.Atoi(strings.TrimPrefix(s, "c:"))
	if err != nil || !strings.HasPrefix(s, "c:") {
		return fmt.Errorf("%q is not a cursor", s)
	}
	c.id = n
	return nil
}

type mSortArgs struct {
	By    mField   `graphql:"by"`
	After *mCursor `graphql:"after"`
}

func newMarshalerSchema(t *testing.T, opts ...SchemaOption) (*Schema, error) {
	t.Helper()
	return NewSchema(SDL(`enum Field { NAME PRICE }
scalar Cursor
type Query {
  sorted(by: Field!, after: Cursor): String!
  fields: [Field!]!
  cursors: [Cursor!]!
  bogus: Field
  silent: Field
}`), append([]SchemaOption{
		EnumMarshaler[mField]("Field"),
		ScalarMarshaler[mCursor]("Cursor"),
		Args[mSortArgs](),
		Query(
			ResolveArgs("sorted", func(_ context.Context, _ Root, a mSortArgs) (string, error) {
				after := "none"
				if a.After != nil {
					after = strconv.Itoa(a.After.id)
				}
				if a.By.less == nil {
					return "", fmt.Errorf("by was not decoded through UnmarshalGQL")
				}
				return a.By.column + " after " + after, nil
			}),
			Resolve("fields", func(context.Context, Root) ([]mField, error) {
				return []mField{{column: "name"}, {column: "price"}}, nil
			}),
			Resolve("cursors", func(context.Context, Root) ([]mCursor, error) {
				return []mCursor{{1}, {22}}, nil
			}),
			Resolve("bogus", func(context.Context, Root) (*mField, error) { return &mField{column: "bogus"}, nil }),
			Resolve("silent", func(context.Context, Root) (*mField, error) { return &mField{column: "silent"}, nil }),
		),
	}, opts...)...)
}

// ent, velox and gqlgen's own types encode themselves with MarshalGQL and
// decode with UnmarshalGQL. Binding them directly is what lets a generated
// resolver take the ORM's order type as its argument, instead of a string
// enum the application converts by hand.
func TestMarshalerTypesBindAsEnumsAndScalars(t *testing.T) {
	s, err := newMarshalerSchema(t)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)

	expectData(t, run(t, e, `{ fields cursors }`, ""), `{"fields":["NAME","PRICE"],"cursors":["c:1","c:22"]}`)
	expectData(t, run(t, e, `{ sorted(by: PRICE, after: "c:7") }`, ""), `{"sorted":"price after 7"}`)
	expectData(t, run(t, e, `query($by: Field!, $after: Cursor) { sorted(by: $by, after: $after) }`, `{"by":"NAME","after":"c:3"}`),
		`{"sorted":"name after 3"}`)

	resp := run(t, e, `query($by: Field!) { sorted(by: $by) }`, `{"by":"COLOR"}`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "COLOR") {
		t.Errorf("an undeclared enum value in a variable: %s", errorsJSON(resp.Errors))
	}
	resp = run(t, e, `{ sorted(by: NAME, after: "x") }`, "")
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "not a cursor") {
		t.Errorf("a scalar UnmarshalGQL refuses: %s", errorsJSON(resp.Errors))
	}

	// Output is checked, not trusted: MarshalGQL naming a value the enum does
	// not declare is a field error, as it is for Enum, and so is writing
	// nothing at all.
	resp = run(t, e, `{ bogus silent }`, "")
	if string(resp.Data) != `{"bogus":null,"silent":null}` || len(resp.Errors) != 2 ||
		!strings.Contains(resp.Errors[0].Message, `"BOGUS"`) || !strings.Contains(resp.Errors[1].Message, "wrote nothing") {
		t.Errorf("bad output: data %s, errors %s", resp.Data, errorsJSON(resp.Errors))
	}
}

func TestMarshalerBindingChecksTheKind(t *testing.T) {
	_, err := NewSchema(SDL(`enum Field { NAME }
scalar Cursor
type Query { f: Field c: Cursor }`),
		EnumMarshaler[mCursor]("Cursor"),
		ScalarMarshaler[mField]("Field"),
	)
	if err == nil || !strings.Contains(err.Error(), `EnumMarshaler "Cursor": type is not an enum`) ||
		!strings.Contains(err.Error(), `ScalarMarshaler "Field": type is not a scalar`) {
		t.Fatalf("err = %v", err)
	}
}

// mRaw writes what it holds, which is what a hand-written MarshalGQL does
// when it forgets a quote or interpolates a value without escaping it.
type mRaw struct{ out string }

func (r mRaw) MarshalGQL(w io.Writer)  { io.WriteString(w, r.out) }
func (*mRaw) UnmarshalGQL(v any) error { return nil }

// ScalarMarshaler is Scalar with the functions taken from the type, and the
// output check comes with it: what MarshalGQL wrote went into the response
// as it was, so one bad value produced a document no client could parse, or
// one with a key the schema never declared, and no error either way.
func TestScalarMarshalerMustWriteOneValue(t *testing.T) {
	var out string
	s, err := NewSchema(SDL(`scalar Raw type Query { ok: String! r: Raw rs: [Raw] }`),
		ScalarMarshaler[mRaw]("Raw"),
		Query(
			Field("ok", func(Root) string { return "x" }),
			Field("r", func(Root) mRaw { return mRaw{out} }),
			Field("rs", func(Root) []mRaw { return []mRaw{{`"a"`}, {out}} }),
		))
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	for _, tc := range []struct{ name, out string }{
		{"a second value and a key", `"a","b":"c"`},
		{"an unquoted string", `123e4567-e89b-12d3-a456-426614174000`},
		{"an unterminated string", `"a`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out = tc.out
			resp := run(t, e, `{ ok r rs }`, "")
			if got := string(resp.Data); got != `{"ok":"x","r":null,"rs":["a",null]}` {
				t.Fatalf("data = %s, want the bad value withheld and its neighbours intact", got)
			}
			if len(resp.Errors) != 2 {
				t.Fatalf("want one error per bad value, got %s", errorsJSON(resp.Errors))
			}
			for _, e := range resp.Errors {
				if e.Message != "scalar Raw: marshal must write exactly one JSON value" {
					t.Errorf("message = %q", e.Message)
				}
			}
		})
	}

	// What an encoder writes is one value and stays one: json.Encoder ends
	// it with a newline.
	out = "{\"k\":[1,2]}\n"
	resp := run(t, e, `{ r }`, "")
	if len(resp.Errors) > 0 || !json.Valid(resp.Data) || !strings.Contains(string(resp.Data), `{"k":[1,2]}`) {
		t.Fatalf("a well-formed value was refused: data %s, errors %s", resp.Data, errorsJSON(resp.Errors))
	}
}
