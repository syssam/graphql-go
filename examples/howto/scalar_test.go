package howto

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
)

// query runs one operation and returns the data and the errors. The data is
// copied out because the response's buffer goes back to a pool.
func query(t *testing.T, schema *graphql.Schema, text, variables string) (string, []*graphql.Error) {
	t.Helper()
	req := &graphql.Request{Query: text}
	if variables != "" {
		req.Variables = json.RawMessage(variables)
	}
	resp := graphql.NewExecutor(schema).Execute(context.Background(), req)
	defer resp.Release()
	return string(resp.Data), resp.Errors
}

// The price is 2^53 + 1 cents, the first whole number a float64 cannot hold.
// It comes back digit for digit only if nothing between the client and
// unmarshalMoney turned it into a float.
const offerData = `{"offer":{"slug":"red-shoes","price":9007199254740993,"until":"2026-12-24T18:00:00+01:00"}}`

func TestScalarsRoundTripAsLiterals(t *testing.T) {
	schema, err := NewOffersSchema()
	if err != nil {
		t.Fatal(err)
	}
	data, errs := query(t, schema, `{
		offer(slug: "red-shoes", price: 9007199254740993, until: "2026-12-24T18:00:00+01:00") {
			slug price until
		}
	}`, "")
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %v", errs[0])
	}
	if data != offerData {
		t.Fatalf("data = %s\nwant   %s", data, offerData)
	}
}

func TestScalarsRoundTripAsVariables(t *testing.T) {
	schema, err := NewOffersSchema()
	if err != nil {
		t.Fatal(err)
	}
	data, errs := query(t, schema, `query($slug: Slug!, $price: Money!, $until: Time!) {
		offer(slug: $slug, price: $price, until: $until) { slug price until }
	}`, `{"slug": "red-shoes", "price": 9007199254740993, "until": "2026-12-24T18:00:00+01:00"}`)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %v", errs[0])
	}
	if data != offerData {
		t.Fatalf("data = %s\nwant   %s", data, offerData)
	}
}

// A value the scalar refuses is the client's error, reported before any
// resolver runs, and it carries what the unmarshal function said.
func TestScalarRefusesABadValue(t *testing.T) {
	schema, err := NewOffersSchema()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ args, want string }{
		"fractional cents": {`slug: "a", price: 1.5, until: "2026-12-24T18:00:00Z"`, "Money must be a whole number of cents, got 1.5"},
		"money as text":    {`slug: "a", price: "1", until: "2026-12-24T18:00:00Z"`, "Money must be a whole number of cents, got string"},
		"not a slug":       {`slug: "Red Shoes", price: 1, until: "2026-12-24T18:00:00Z"`, "Slug must be lower-case words joined by hyphens"},
		"not a time":       {`slug: "a", price: 1, until: "tomorrow"`, "Time must be an RFC 3339 string"},
	} {
		t.Run(name, func(t *testing.T) {
			data, errs := query(t, schema, `{ offer(`+c.args+`) { slug } }`, "")
			if len(errs) != 1 || !strings.Contains(errs[0].Message, c.want) {
				t.Fatalf("errors = %v, want one saying %q", errs, c.want)
			}
			if data != "" {
				t.Fatalf("a refused argument still produced data: %s", data)
			}
		})
	}
}

// tallySchema binds Money with the given marshal function and an unmarshal
// that records every value it is handed, so a test can see what reached it
// and how often.
func tallySchema(t *testing.T, marshal func(*graphql.Writer, Money) error) (*graphql.Schema, *[]any) {
	t.Helper()
	var seen []any
	schema, err := graphql.NewSchema(
		graphql.SDL(`scalar Money type Query { twice(amount: Money): Money }`),
		graphql.Scalar("Money", marshal, func(v any) (Money, error) {
			seen = append(seen, v)
			return unmarshalMoney(v)
		}),
		graphql.Query(
			graphql.FieldArgs("twice", func(_ graphql.Root, a struct{ Amount *Money }) *Money {
				if a.Amount == nil {
					return nil
				}
				doubled := *a.Amount * 2
				return &doubled
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return schema, &seen
}

// The reason an unmarshal function has to be pure and cheap: for a variable
// it runs once to check the value before execution and once to decode it.
func TestUnmarshalRunsTwiceForAVariableAndOnceForALiteral(t *testing.T) {
	schema, seen := tallySchema(t, marshalMoney)

	data, errs := query(t, schema, `{ twice(amount: 21) }`, "")
	if len(errs) > 0 || data != `{"twice":42}` {
		t.Fatalf("literal: data = %s, errors = %v", data, errs)
	}
	if len(*seen) != 1 {
		t.Fatalf("a literal reached unmarshal %d times, want 1: %v", len(*seen), *seen)
	}

	*seen = nil
	data, errs = query(t, schema, `query($amount: Money) { twice(amount: $amount) }`, `{"amount": 21}`)
	if len(errs) > 0 || data != `{"twice":42}` {
		t.Fatalf("variable: data = %s, errors = %v", data, errs)
	}
	if len(*seen) != 2 {
		t.Fatalf("a variable reached unmarshal %d times, want 2: %v", len(*seen), *seen)
	}
	for _, v := range *seen {
		if _, ok := v.(json.Number); !ok {
			t.Fatalf("unmarshal was handed a %T, want json.Number", v)
		}
	}
}

func TestNullNeverReachesUnmarshal(t *testing.T) {
	schema, seen := tallySchema(t, marshalMoney)
	for _, c := range [][2]string{
		{`{ twice(amount: null) }`, ""},
		{`query($amount: Money) { twice(amount: $amount) }`, `{"amount": null}`},
	} {
		data, errs := query(t, schema, c[0], c[1])
		if len(errs) > 0 || data != `{"twice":null}` {
			t.Fatalf("%s: data = %s, errors = %v", c[0], data, errs)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("unmarshal was called for a null: %v", *seen)
	}
}

// Writing nothing, or two values, would corrupt the whole response. The
// engine checks, discards what was written and fails the one field.
func TestMarshalMustWriteExactlyOneValue(t *testing.T) {
	for name, marshal := range map[string]func(*graphql.Writer, Money) error{
		"nothing":    func(*graphql.Writer, Money) error { return nil },
		"two values": func(w *graphql.Writer, m Money) error { w.Int64(int64(m)); w.String("cents"); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			schema, _ := tallySchema(t, marshal)
			data, errs := query(t, schema, `{ twice(amount: 21) }`, "")
			if data != `{"twice":null}` {
				t.Fatalf("data = %s, want the field nulled", data)
			}
			const want = "scalar Money: marshal must write exactly one JSON value"
			if len(errs) != 1 || errs[0].Message != want {
				t.Fatalf("errors = %v, want %q", errs, want)
			}
		})
	}
}

// cents is a second Go type for the same scalar, as a schema has when one
// package holds amounts as an int64 and another as a type of its own.
type cents int64

func TestOneScalarTakesSeveralGoTypesButEachOnlyOnce(t *testing.T) {
	const sdl = `scalar Money type Query { a: Money b: Money }`
	fields := graphql.Query(
		graphql.Field("a", func(graphql.Root) Money { return 1 }),
		graphql.Field("b", func(graphql.Root) cents { return 2 }),
	)
	marshalCents := func(w *graphql.Writer, c cents) error { w.Int64(int64(c)); return nil }
	unmarshalCents := func(v any) (cents, error) { m, err := unmarshalMoney(v); return cents(m), err }

	schema, err := graphql.NewSchema(graphql.SDL(sdl), fields,
		graphql.Scalar("Money", marshalMoney, unmarshalMoney),
		graphql.Scalar("Money", marshalCents, unmarshalCents),
	)
	if err != nil {
		t.Fatalf("two Go types for one scalar: %v", err)
	}
	if data, errs := query(t, schema, `{ a b }`, ""); len(errs) > 0 || data != `{"a":1,"b":2}` {
		t.Fatalf("data = %s, errors = %v", data, errs)
	}

	_, err = graphql.NewSchema(graphql.SDL(sdl), fields,
		graphql.Scalar("Money", marshalMoney, unmarshalMoney),
		graphql.Scalar("Money", marshalMoney, unmarshalMoney),
		graphql.Scalar("Money", marshalCents, unmarshalCents),
	)
	if want := `Scalar "Money": Go type howto.Money is bound more than once`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("binding Money twice: err = %v, want %q", err, want)
	}
}
