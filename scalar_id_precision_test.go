package graphql

import (
	"context"
	"strconv"
	"testing"
)

// An ID bound to an int64 has to survive the full int64 range exactly.
// Snowflake ids, database bigints and anything else above 2^53 are ordinary,
// and a value that arrives one off is not an error anywhere: it is a lookup
// for the wrong row.
//
// rawInt64 gets this right by trying json.Number.Int64 before falling back to
// Float64, so the whole int64 range survives whichever form the value takes:
// an inline literal, a JSON number, or the JSON string most clients actually
// send.
//
// This is not a regression test for the fieldArguments change, and saying so
// matters because it looks like one. An integer literal always reached the
// decoder exactly -- gqlparser parses IntValue with ParseInt, so it was an
// int64 before that change and a json.Number carrying the same digits after.
// What that change fixed was a *float* literal being rounded through float64
// (9007199254740993.0 arriving as 9.007199254740992e+15) and an integer
// literal wider than int64 panicking. Neither is this file's subject; both are
// covered in coerce_literal_variable_test.go.

type idPrecArgs struct{ ID int64 }

func idPrecisionExec(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`type Query { row(id: ID!): String! }`),
		Args[idPrecArgs](InputField("id", func(a *idPrecArgs, v int64) { a.ID = v })),
		Query(ResolveArgs("row", func(_ context.Context, _ Root, a idPrecArgs) (string, error) {
			return strconv.FormatInt(a.ID, 10), nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

func TestAnIDKeepsFullInt64PrecisionAsALiteralAndAsAVariable(t *testing.T) {
	e := idPrecisionExec(t)
	for _, id := range []string{
		"9007199254740993",    // 2^53 + 1: the first integer float64 cannot hold
		"9223372036854775807", // MaxInt64
		"-9223372036854775808",
		"1234567890123456789", // a plausible snowflake
		"0",
		"-1",
	} {
		t.Run(id, func(t *testing.T) {
			want := `{"row":"` + id + `"}`

			lit := run(t, e, `{ row(id: `+id+`) }`, "")
			if len(lit.Errors) > 0 {
				t.Fatalf("literal: %s", errorsJSON(lit.Errors))
			}
			if got := string(lit.Data); got != want {
				t.Errorf("literal %s decoded to %s, want %s; the digits were lost on the "+
					"way in, so this is a lookup for a different row", id, got, want)
			}

			// As a JSON number.
			num := run(t, e, `query($id: ID!){ row(id: $id) }`, `{"id":`+id+`}`)
			if len(num.Errors) > 0 {
				t.Fatalf("numeric variable: %s", errorsJSON(num.Errors))
			}
			if got := string(num.Data); got != want {
				t.Errorf("numeric variable %s decoded to %s, want %s", id, got, want)
			}

			// And as a JSON string, which is how most clients send an ID.
			str := run(t, e, `query($id: ID!){ row(id: $id) }`, `{"id":"`+id+`"}`)
			if len(str.Errors) > 0 {
				t.Fatalf("string variable: %s", errorsJSON(str.Errors))
			}
			if got := string(str.Data); got != want {
				t.Errorf("string variable %s decoded to %s, want %s", id, got, want)
			}
		})
	}
}
