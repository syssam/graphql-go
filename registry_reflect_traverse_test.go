package graphql

import (
	"context"
	"strings"
	"testing"
)

// reflectTraverse is the one reflection loop CLAUDE.md allows on the request
// path, used for any list shape the registry has no typed loop for. In
// practice that means a *nested* list: registerObjectShapes and
// registerAbstractShapes cover []E, []*E and the iter.Seq forms, but not
// [][]E. The real consumer schema reaches it (`type=[][]velox.Noder`), so it
// is not a corner.
//
// Its contract is one line -- stop when yield returns false -- and it is only
// reachable on the *sequential* path. writeList's own comment says why: with
// concurrency the list is drained first, and drainList's callback always
// returns true because draining has to see every element. A test that puts a
// Resolve field under the list therefore measures the drain and proves
// nothing about the early exit, because WithMaxConcurrency defaults to
// GOMAXPROCS*4 and the deeply-schedulable path is live unless the fields
// beneath the list are pure. Both tests below use pure fields for that reason.

type rtNode struct {
	ID   string
	Fail bool
}

// rtSeen records the elements the traversal actually reached, in order. Safe
// to use unsynchronised precisely because the fields below are pure, so the
// list is written sequentially.
var rtSeen []string

func reflectListExec(t *testing.T, rows [][]*rtNode, opts ...ExecutorOption) *Executor {
	t.Helper()
	rtSeen = nil
	s, err := NewSchema(SDL(`
		type Row { id: ID! label: String! }
		type Query { grid: [[Row!]!]! }
	`),
		Object[rtNode]("Row",
			Field("id", func(n *rtNode) ID { return ID(n.ID) }),
			// Pure, so nothing beneath the list is schedulable and writeList
			// stays a single pass over shape.traverse.
			Field("label", func(n *rtNode) string {
				rtSeen = append(rtSeen, n.ID)
				return strings.Repeat("x", 64)
			}),
		),
		Query(Resolve("grid", func(context.Context, Root) ([][]*rtNode, error) { return rows, nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s, opts...)
}

// A nested list under a response-byte limit is the shape that matters for a
// hostile client: once the limit trips, no further element may be resolved or
// written, however long a list the client asked for.
//
// Be clear about what this does and does not guard. It pins the *work* -- the
// field accessor is not called for the rows after the trip -- and that is
// held by writeField's own checkpoint (`if w.OverLimit() { return false }`),
// which returns before reaching the accessor. It does **not** guard
// reflectTraverse's `if !yield(...) { return }`: deleting that line leaves
// this test passing, because the loop then runs to the end of the slice
// calling a writeElem that short-circuits immediately. The early exit is a
// performance guard with no observable behaviour -- the same category as
// gqlwsproto's idle Stop -- and there is no behavioural test to write for it.
// Do not delete it on the strength of that; do not add a test expecting one to
// be possible either.
func TestReflectTraverseStopsOnAResponseLimitTrip(t *testing.T) {
	rows := make([][]*rtNode, 500)
	for i := range rows {
		rows[i] = []*rtNode{{ID: "r"}}
	}
	e := reflectListExec(t, rows, WithMaxResponseBytes(2048))
	resp := run(t, e, `{ grid { id label } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatalf("the limit was never tripped; data is %d bytes", len(resp.Data))
	}
	if n := len(rtSeen); n >= len(rows) {
		t.Errorf("%d of %d rows were resolved after the response limit tripped; "+
			"a client picks the list length, so the work has to stop at the trip",
			n, len(rows))
	}
	t.Logf("stopped after %d of %d rows", len(rtSeen), len(rows))
}

// And the control: with no limit the same query reaches every row, so the test
// above is measuring an early exit rather than a traversal that never got
// going.
func TestReflectTraverseReachesEveryRowWithoutALimit(t *testing.T) {
	rows := make([][]*rtNode, 50)
	for i := range rows {
		rows[i] = []*rtNode{{ID: "r"}}
	}
	e := reflectListExec(t, rows)
	resp := run(t, e, `{ grid { id label } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if n := len(rtSeen); n != len(rows) {
		t.Errorf("the traversal reached %d of %d rows with no limit", n, len(rows))
	}
}
