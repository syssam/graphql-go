# Add a custom scalar

Teach the engine how a Go type is written to and read from a scalar the schema declares.
There are three ways, and this page uses one of each.

The schema declares the scalars by name. `offer` returns its arguments, so one query sends
each scalar in and reads it back out.

```go
const offersSDL = `
scalar Money
scalar Slug
scalar Time

type Offer { slug: Slug! price: Money! until: Time! }

type Query {
  offer(slug: Slug!, price: Money!, until: Time!): Offer!
}
`
```

**A small type of your own** needs a function each way. The marshal function writes one
JSON value through the `Writer`, and the unmarshal function turns what the client sent
into the Go type or says why it cannot.

```go
type Money int64

// marshalMoney writes one JSON value, which is the whole contract of a
// marshal function. Each Writer method writes exactly one.
func marshalMoney(w *graphql.Writer, m Money) error {
	w.Int64(int64(m))
	return nil
}

// unmarshalMoney is given what the client sent. A number arrives as a
// json.Number holding the client's own digits, from a literal in the query
// and from a variable alike, so nothing has been rounded on the way in.
func unmarshalMoney(v any) (Money, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("Money must be a whole number of cents, got %T", v)
	}
	cents, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("Money must be a whole number of cents, got %s", n)
	}
	return Money(cents), nil
}
```

**A type that already has `MarshalGQL` and `UnmarshalGQL`** needs nothing written. Those
are gqlgen's methods, and the ones an ORM's generated types carry.

```go
type Slug string

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// MarshalGQL writes a JSON string. json.Marshal rather than quotes around
// the text, because what is written here goes into the response as it is.
func (s Slug) MarshalGQL(w io.Writer) {
	b, _ := json.Marshal(string(s))
	_, _ = w.Write(b)
}

func (s *Slug) UnmarshalGQL(v any) error {
	text, ok := v.(string)
	if !ok || !slugPattern.MatchString(text) {
		return fmt.Errorf("Slug must be lower-case words joined by hyphens, got %v", v)
	}
	*s = Slug(text)
	return nil
}
```

**`time.Time`** is built in, as an RFC 3339 string. All three are bound by name, one
schema option each.

```go
// A small type of your own: one function each way.
graphql.Scalar("Money", marshalMoney, unmarshalMoney),
// A type that has MarshalGQL and UnmarshalGQL: nothing to write.
graphql.ScalarMarshaler[Slug]("Slug"),
// time.Time as an RFC 3339 string: built in, under the name the
// schema chose.
graphql.Time("Time"),
```

Nothing else mentions a scalar. A field whose Go type is `Money` is written with
`marshalMoney`, and an argument whose Go type is `Money` is read with `unmarshalMoney`.

## Check it worked

Send each scalar in and read it back, once as literals in the query and once as variables,
because the two take different paths to the unmarshal function. `query` executes one
operation and returns the data and the errors. The price is 2^53 + 1, the first whole
number a float64 cannot hold, so it comes back digit for digit only if nothing turned it
into a float on the way.

```go
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
```

`TestScalarsRoundTripAsLiterals` is the same query with the values written into it, and
`TestScalarRefusesABadValue` checks that `1.5`, `"Red Shoes"` and `"tomorrow"` are errors.

```sh
go test -race -run 'Scalar|Marshal|Unmarshal' ./examples/howto/
```

## What goes wrong

- **A field is null, with the error `scalar Money: marshal must write exactly one JSON
  value`.** The marshal function wrote nothing, or wrote twice. The engine checks, discards
  what was written and fails that field. Call one `Writer` method, once.
- **Every number is refused, though unmarshal has a `case int64` or a `case float64`.** A
  number arrives as a `json.Number` holding the client's digits, from a literal and from a
  variable alike. Switch on `json.Number` and `string`.
- **A log line or a counter in unmarshal fires twice per request.** For a variable it runs
  once to check the value before execution and once to decode it, so keep it pure and
  cheap. A `null` never reaches it, so it needs no case for one.
- **`NewSchema` fails: `Scalar "Money": Go type howto.Money is bound more than once`.** The
  same Go type was bound to the scalar twice. Several different Go types on one scalar is
  fine; the same one twice is not.

## Next

- [`examples/storefront/storefront.go`](../../examples/storefront/storefront.go): a `Money`
  written as a decimal string, and scalars inside input objects.
- [`scalar.go`](../../scalar.go): the godoc of `Scalar` and `Time`, including what `Time`
  does with the zero time.
