package graphql

import (
	"context"
	"strings"
	"testing"
	"time"
)

type timeArgs struct {
	At time.Time `graphql:"at"`
}

// Time is gqlgen's wire format, so a service moving over answers its clients
// with the same bytes: RFC 3339, the value's own offset kept, nanoseconds
// written only as far as they are non-zero.
func TestTimeIsRFC3339(t *testing.T) {
	plus8 := time.FixedZone("", 8*3600)
	at := time.Date(2026, 9, 25, 14, 30, 0, 120000000, plus8)
	var zero time.Time
	s, err := NewSchema(SDL(`scalar Time
type Query {
  at: Time!
  whole: Time!
  zero: Time!
  missing: Time
  present: Time
  list: [Time!]!
  far: Time
  echo(at: Time!): String!
}`),
		Time("Time"),
		Args[timeArgs](),
		Query(
			Resolve("at", func(context.Context, Root) (time.Time, error) { return at, nil }),
			Resolve("whole", func(context.Context, Root) (time.Time, error) { return at.Truncate(time.Second), nil }),
			Resolve("zero", func(context.Context, Root) (time.Time, error) { return zero, nil }),
			Resolve("missing", func(context.Context, Root) (*time.Time, error) { return nil, nil }),
			Resolve("present", func(context.Context, Root) (*time.Time, error) { return &at, nil }),
			Resolve("far", func(context.Context, Root) (time.Time, error) {
				return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), nil
			}),
			Resolve("list", func(context.Context, Root) ([]time.Time, error) { return []time.Time{at.UTC()}, nil }),
			ResolveArgs("echo", func(_ context.Context, _ Root, a timeArgs) (string, error) {
				return a.At.UTC().Format(time.RFC3339Nano), nil
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)

	expectData(t, run(t, e, `{ at whole zero missing present list }`, ""),
		`{"at":"2026-09-25T14:30:00.12+08:00","whole":"2026-09-25T14:30:00+08:00","zero":"0001-01-01T00:00:00Z",`+
			`"missing":null,"present":"2026-09-25T14:30:00.12+08:00","list":["2026-09-25T06:30:00.12Z"]}`)

	resp := run(t, e, `{ far }`, "")
	if string(resp.Data) != `{"far":null}` || len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "year 10000") {
		t.Errorf("a year RFC 3339 cannot hold: data %s, errors %s", resp.Data, errorsJSON(resp.Errors))
	}

	for _, in := range []string{`"2026-09-25T14:30:00+08:00"`, `"2026-09-25T06:30:00Z"`, `"2026-09-25T06:30:00.000Z"`} {
		expectData(t, run(t, e, `{ echo(at: `+in+`) }`, ""), `{"echo":"2026-09-25T06:30:00Z"}`)
	}
	expectData(t, run(t, e, `query($at: Time!) { echo(at: $at) }`, `{"at":"2026-09-25T14:30:00.5+08:00"}`),
		`{"echo":"2026-09-25T06:30:00.5Z"}`)

	// gqlgen also reads "" as the zero time and a zoneless date-time as UTC.
	// Both are refused: the first hides a missing value, and the second is a
	// wall-clock time each client means in its own zone.
	for _, in := range []string{`""`, `"2026-09-25 06:30:00"`, `"2026-09-25"`, `1758781800`} {
		resp = run(t, e, `{ echo(at: `+in+`) }`, "")
		if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "RFC 3339") {
			t.Errorf("echo(at: %s): want one error naming RFC 3339, got %s", in, errorsJSON(resp.Errors))
		}
	}
}
