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

// Whether a literal argument is acceptable is a fact about the document. It
// was checked only where the plan decodes it, which is in the variants that
// include its field: the same query text was a validation error with one
// value of a variable and ran with the other, and behind @skip(if: true) the
// literal was never looked at.
func TestALiteralArgumentIsValidatedInEveryVariant(t *testing.T) {
	_, fixture := newFixtureExecutor(t)
	dup := NewExecutor(newDupErrSchema(t))
	for _, tc := range []struct {
		name  string
		e     *Executor
		query string
		want  int
	}{
		{"a skipped field", fixture, `query($s: Boolean!) { strings a: echo(v: 2147483648) @skip(if: $s) }`, 1},
		{"an included field", fixture, `query($s: Boolean!) { strings a: echo(v: 2147483648) @include(if: $s) }`, 1},
		{"a skipped inline fragment", fixture, `query($s: Boolean!) { strings ... @skip(if: $s) { a: echo(v: 2147483648) } }`, 1},
		{"a skipped spread", fixture, `query($s: Boolean!) { strings ...F @skip(if: $s) } fragment F on Query { a: echo(v: 2147483648) }`, 1},
		{"beneath a skipped field", dup, `query($s: Boolean!) { root @skip(if: $s) { id at(day: "Funday") } }`, 3},
		{"on an abstract parent", dup, `query($s: Boolean!) { root { id at(day: "Funday") @skip(if: $s) } }`, 3},
		{"one type of an abstract parent", dup, `query($s: Boolean!) { root { id ... on D1 @skip(if: $s) { at(day: "Funday") } } }`, 1},
		// The two selections are the same once the skipped spreads are gone,
		// and the second must still be looked at.
		{"skipped spreads that leave equal selections", dup, `query($s: Boolean!) { a: root { ...A ...F @skip(if: $s) } b: root { ...A ...G @skip(if: $s) } }
			fragment A on Node { id } fragment F on Node { id } fragment G on Node { at(day: "Funday") }`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := run(t, tc.e, tc.query, `{"s":true}`)
			off := run(t, tc.e, tc.query, `{"s":false}`)
			if !on.HasRequestErrors() || !off.HasRequestErrors() {
				t.Fatalf("s=true: data=%s errors=%s\ns=false: data=%s errors=%s",
					on.Data, errorsJSON(on.Errors), off.Data, errorsJSON(off.Errors))
			}
			if got, want := errorsJSON(on.Errors), errorsJSON(off.Errors); got != want {
				t.Fatalf("the error depends on the variable:\ns=true:  %s\ns=false: %s", got, want)
			}
			if len(on.Errors) != tc.want || on.Errors[0].Extensions["code"] != CodeValidationFailed {
				t.Fatalf("errors = %s, want %d with code %s", errorsJSON(on.Errors), tc.want, CodeValidationFailed)
			}
		})
	}

	// A condition written as a literal excludes the field from every plan.
	if resp := run(t, fixture, `{ strings a: echo(v: 2147483648) @skip(if: true) }`, ""); !resp.HasRequestErrors() {
		t.Fatalf("an out-of-range literal behind @skip(if: true) ran: %s", resp.Data)
	}
	// And what is acceptable stays acceptable, skipped or not.
	for _, vars := range []string{`{"s":true}`, `{"s":false}`} {
		if resp := run(t, fixture, `query($s: Boolean!) { strings a: echo(v: 1) @skip(if: $s) }`, vars); len(resp.Errors) > 0 {
			t.Fatalf("%s: %s", vars, errorsJSON(resp.Errors))
		}
	}
}
