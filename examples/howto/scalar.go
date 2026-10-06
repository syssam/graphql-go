package howto

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/syssam/graphql-go"
)

const offersSDL = `
scalar Money
scalar Slug
scalar Time

type Offer { slug: Slug! price: Money! until: Time! }

type Query {
  offer(slug: Slug!, price: Money!, until: Time!): Offer!
}
`

// Money is an amount in cents, and an integer on the wire: an int64, so an
// amount is the number the client sent and not the nearest float to it.
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

// Slug encodes itself: MarshalGQL and UnmarshalGQL are the methods gqlgen
// defines, and the ones an ORM's generated types carry.
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

type Offer struct {
	Slug  Slug
	Price Money
	Until time.Time
}

func NewOffersSchema() (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(offersSDL),
		// A small type of your own: one function each way.
		graphql.Scalar("Money", marshalMoney, unmarshalMoney),
		// A type that has MarshalGQL and UnmarshalGQL: nothing to write.
		graphql.ScalarMarshaler[Slug]("Slug"),
		// time.Time as an RFC 3339 string: built in, under the name the
		// schema chose.
		graphql.Time("Time"),

		graphql.Object[Offer]("Offer",
			graphql.Field("slug", func(o *Offer) Slug { return o.Slug }),
			graphql.Field("price", func(o *Offer) Money { return o.Price }),
			graphql.Field("until", func(o *Offer) time.Time { return o.Until }),
		),
		graphql.Query(
			// The arguments come back as the result, so one query sends
			// each scalar in and reads it out again.
			graphql.FieldArgs("offer", func(_ graphql.Root, a offerArgs) *Offer {
				return &Offer{Slug: a.Slug, Price: a.Price, Until: a.Until}
			}),
		),
	)
}

// offerArgs is decoded from the field's arguments by name: Slug from slug,
// Price from price. Each field's Go type picks the scalar binding that
// decodes it.
type offerArgs struct {
	Slug  Slug
	Price Money
	Until time.Time
}
