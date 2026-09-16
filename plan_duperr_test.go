package graphql

import (
	"fmt"
	"testing"
)

// dupErrSDL puts an argument whose scalar fails to decode on an interface
// field, so every concrete type's expansion of that field hits the same
// failure. Three types are enough to observe duplication.
const dupErrSDL = `
scalar Weekday
interface Node { id: ID! at(day: Weekday!): String }
type D0 implements Node { id: ID! at(day: Weekday!): String }
type D1 implements Node { id: ID! at(day: Weekday!): String }
type D2 implements Node { id: ID! at(day: Weekday!): String }
type Query { root: Node }
`

type weekday string

type dupTag0 struct{}
type dupTag1 struct{}
type dupTag2 struct{}

type dupT[T any] struct{ ID string }

func (d *dupT[T]) dupNodeID() string { return d.ID }

type dupNode interface{ dupNodeID() string }

type dayArgs struct {
	Day weekday
}

func dupObject[T any](name string) SchemaOption {
	return Object[dupT[T]](name,
		Field("id", func(d *dupT[T]) string { return d.ID }),
		FieldArgs("at", func(d *dupT[T], a dayArgs) string { return string(a.Day) }),
	)
}

func newDupErrSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(dupErrSDL),
		Scalar("Weekday",
			func(w *Writer, v weekday) error { w.String(string(v)); return nil },
			func(v any) (weekday, error) {
				s, ok := v.(string)
				if !ok || s == "Funday" {
					return "", fmt.Errorf("not a weekday: %v", v)
				}
				return weekday(s), nil
			}),
		Args[dayArgs](),
		dupObject[dupTag0]("D0"),
		dupObject[dupTag1]("D1"),
		dupObject[dupTag2]("D2"),
		Object[Root]("Query", Field("root", func(Root) dupNode { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPlanArgErrorPerConcreteType pins how many errors one bad literal
// produces when it sits under an abstract parent.
//
// This count is unchanged by memoizing compileSelection on (parent type,
// selection set). The memo only collapses calls that recur with an identical
// key; D0, D1 and D2 are three distinct *objectType values (abstractType.possible
// holds one per implementing type), so the abstract branch calls
// compileSelection(D0, ...), compileSelection(D1, ...) and compileSelection(D2, ...)
// exactly once each, both before and after memoization — dispatching a field
// to every possible concrete type is required GraphQL behavior, not the
// redundant re-expansion the memo targets. That redundancy only arises when
// the *same* (parent, selection) pair is reached by more than one path, which
// is what TestFanOutExpansionIsBounded (plan_fanout_test.go) exercises via a
// self-referential interface. Confirmed by instrumenting buildField: it is
// invoked three times, once per obj pointer, both before and after this
// change.
func TestPlanArgErrorPerConcreteType(t *testing.T) {
	s := newDupErrSchema(t)
	e := NewExecutor(s)
	entry, errs := e.document(`{ root { at(day: "Funday") } }`)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	_, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if len(perrs) != 3 {
		t.Fatalf("got %d errors, want 3 (one per concrete type)", len(perrs))
	}
}
