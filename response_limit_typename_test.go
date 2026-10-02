package graphql

import (
	"fmt"
	"iter"
	"strings"
	"testing"
)

type typenameRow struct{ ID string }

// __typename took a shortcut past the checkpoint every other field reports
// its bytes at, so a selection made only of it never reported any: a lazy list
// was pulled to its end and the writer grew to 525 times the limit before the
// final check refused the response. The client sets the amplification, since
// aliases of __typename are free in the cost model.
func TestTypenameOnlySelectionStopsAtTheResponseLimit(t *testing.T) {
	const rows, limit = 5000, 64 << 10
	pulled := 0
	s, err := NewSchema(SDL(`type Row { id: ID! } type Query { rows: [Row!]! }`),
		Object[typenameRow]("Row", Field("id", func(r *typenameRow) ID { return ID(r.ID) })),
		Query(Field("rows", func(Root) iter.Seq[*typenameRow] {
			return func(yield func(*typenameRow) bool) {
				for range rows {
					pulled++
					if !yield(&typenameRow{ID: "1"}) {
						return
					}
				}
			}
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithMaxResponseBytes(limit))

	var q strings.Builder
	q.WriteString("{ rows { ")
	for i := range 500 {
		fmt.Fprintf(&q, "t%d: __typename ", i)
	}
	q.WriteString("} }")

	resp := run(t, e, q.String(), "")
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeResponseTooLarge {
		t.Fatalf("errors = %s, want one RESPONSE_TOO_LARGE", errorsJSON(resp.Errors))
	}
	// A row is about 7 KB here, so the limit is passed within ten of them; the
	// bound leaves room for the row in flight and no more.
	if pulled > 20 {
		t.Fatalf("%d of %d rows were pulled after a %d-byte limit was passed", pulled, rows, limit)
	}
}
