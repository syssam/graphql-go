package graphql

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

type sharedListArgs struct{ List []int }

type sharedPage struct{ Limit int }

type sharedPageArgs struct{ Page *sharedPage }

type sharedScalarArgs struct {
	First *int
	Name  string
}

func sharedArgsExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		input Page { limit: Int! = 0 }
		type Query {
			first(list: [Int!]!): Int!
			limit(page: Page): Int!
			scalar(first: Int, name: String! = "n"): Int!
		}`),
		Args[sharedListArgs](),
		Input[sharedPage]("Page"),
		Args[sharedPageArgs](),
		Args[sharedScalarArgs](),
		Query(
			// Both resolvers do what resolvers do with arguments of their own:
			// normalise them in place.
			FieldArgs("first", func(_ Root, a sharedListArgs) int {
				head := a.List[0]
				slices.Sort(a.List)
				return head
			}),
			FieldArgs("limit", func(_ Root, a sharedPageArgs) int {
				if a.Page.Limit == 0 {
					a.Page.Limit = 20
					return 0
				}
				return a.Page.Limit
			}),
			FieldArgs("scalar", func(_ Root, a sharedScalarArgs) int { return *a.First }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

// Literal arguments are decoded once at plan compile and the plan is shared,
// so a slice or a nested input object in them was one value handed to every
// request: a resolver that sorted or defaulted its own arguments changed the
// next request's, and two at once raced. The same query sent with variables
// decoded per request and did neither.
func TestLiteralArgumentsAreNotSharedBetweenRequests(t *testing.T) {
	e := sharedArgsExecutor(t)
	for _, tc := range []struct{ name, query, want string }{
		{"list", `{ first(list: [3, 1, 2]) }`, `{"first":3}`},
		{"input object", `{ limit(page: {}) }`, `{"limit":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectData(t, run(t, e, tc.query, ""), tc.want)
			expectData(t, run(t, e, tc.query, ""), tc.want)
		})
	}
}

type aliasClampArgs struct{ First *int }

type aliasListArgs struct{ List []int }

type aliasOmitArgs struct{ N Omittable[*int] }

type aliasBag map[string]int

type aliasBagArgs struct{ B aliasBag }

// aliasArgsExecutor's resolvers each write through what their arguments hold,
// in the ways resolvers do -- clamp a page size, sort a list -- and answer
// with what they were handed before the write.
func aliasArgsExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		scalar Bag
		type Query {
			clamp(first: Int): Int!
			single(list: [Int!]!): Int!
			defaulted(list: [Int!]! = [3, 1, 2]): Int!
			omit(n: Int): Int!
			bag(b: Bag!): Int!
		}`),
		Scalar("Bag",
			func(w *Writer, v aliasBag) error { w.Int64(int64(len(v))); return nil },
			func(v any) (aliasBag, error) {
				n, ok := v.(json.Number)
				if !ok {
					return nil, fmt.Errorf("not a number: %v", v)
				}
				i, err := n.Int64()
				return aliasBag{"n": int(i)}, err
			}),
		Args[aliasClampArgs](),
		Args[aliasListArgs](),
		Args[aliasOmitArgs](),
		Args[aliasBagArgs](),
		Query(
			FieldArgs("clamp", func(_ Root, a aliasClampArgs) int {
				v := *a.First
				*a.First = min(v, 100)
				return v
			}),
			FieldArgs("single", func(_ Root, a aliasListArgs) int {
				v := a.List[0]
				a.List[0] = -1
				return v
			}),
			FieldArgs("defaulted", func(_ Root, a aliasListArgs) int {
				v := a.List[0]
				slices.Sort(a.List)
				return v
			}),
			FieldArgs("omit", func(_ Root, a aliasOmitArgs) int {
				p := a.N.Value()
				v := *p
				*p = min(v, 100)
				return v
			}),
			FieldArgs("bag", func(_ Root, a aliasBagArgs) int {
				v := a.B["n"]
				a.B["n"] = -1
				return v
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

// A resolver receives its argument struct by value, and a copy of a struct
// still points wherever the original did. A nullable scalar binds as a
// pointer, so first: 500 written in the query was one int every request
// shared: the resolver that clamped its own page size clamped the next
// request's, and two at once raced. The same holds for anything else a
// literal can decode into that a copy does not own -- a single value coerced
// to a list, a default that is a list, an Omittable around a pointer, a
// custom scalar that decodes to a map.
func TestALiteralArgumentACopySharesIsDecodedPerRequest(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{"a nullable scalar", `{ clamp(first: 500) }`, `{"clamp":500}`},
		{"a value coerced to a list", `{ single(list: 7) }`, `{"single":7}`},
		{"a default that is a list", `{ defaulted }`, `{"defaulted":3}`},
		{"an Omittable pointer", `{ omit(n: 500) }`, `{"omit":500}`},
		{"a scalar that decodes to a map", `{ bag(b: 5) }`, `{"bag":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := aliasArgsExecutor(t)
			expectData(t, run(t, e, tc.query, ""), tc.want)
			expectData(t, run(t, e, tc.query, ""), tc.want)

			// At once as well, so that -race sees a write the plan shares
			// even where the answers happen to agree.
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					for range 50 {
						resp := run(t, e, tc.query, "")
						if got := string(resp.Data); got != tc.want || len(resp.Errors) > 0 {
							t.Errorf("data = %s, want %s (%s)", got, tc.want, errorsJSON(resp.Errors))
							return
						}
						resp.Release()
					}
				})
			}
			wg.Wait()
		})
	}
}

// What the fix must not cost. Arguments that hold nothing a copy would share
// are still decoded once, at plan compile, and shared; it is decided on the
// value, so an optional argument the query leaves out costs nothing either.
// And where all that a copy would share is what top-level pointers to scalars
// point at -- first: 5 into a *int, the commonest argument there is -- the
// decoded value is copied for the request with pointees of its own, not
// decoded again. Only what is left takes the decode.
func TestLiteralArgumentsAreDecodedAgainOnlyWhereACopyWillNotDo(t *testing.T) {
	type mode struct{ perRequest, copied bool }
	shared, copied, decoded := mode{}, mode{true, true}, mode{true, false}
	e := sharedArgsExecutor(t)
	for query, want := range map[string]mode{
		`{ scalar(name: "x") }`:           shared,
		`{ scalar(first: 2, name: "x") }`: copied,
		`{ first(list: [1]) }`:            decoded,
		`{ limit(page: {limit: 1}) }`:     decoded,
		// The type can hold a nested object; this query's value does not.
		`{ limit }`: shared,
	} {
		oc, errs := e.prepareOperation(&Request{Query: query}, false)
		if errs != nil {
			t.Fatalf("%s: %v", query, errs[0])
		}
		f := oc.plan.sel.fields[0]
		if got := (mode{f.dynamicArgs, f.freshArgs}); got != want {
			t.Errorf("%s: per request = %t, by copying = %t; want %t and %t", query, got.perRequest, got.copied, want.perRequest, want.copied)
		}
		if f.args == nil {
			t.Errorf("%s: no arguments were decoded at plan compile", query)
		}
	}
}

type copyID string

type copyKind int

type copyStamp struct {
	At   int64
	Zone [2]byte
}

type copyArgs struct {
	First *int
	Last  *int
	Name  *string
	On    *bool
	Ratio *float64
	ID    *copyID
	Kind  *copyKind
	Stamp *copyStamp
	Plain int
	// hidden is no argument and stays nil; it is here because a struct with
	// a field reflection may not set must still copy.
	hidden *int
}

type copyHandArgs struct{ First, Also *int }

type copyBag map[string]int

type copyBagArgs struct{ B *copyBag }

type copyCountedArgs struct{ First *int }

type copyWrappedArgs struct {
	N Omittable[*int]
}

type copyAnyArgs struct{ V any }

// copyCountedSets counts the calls of a setter written by hand.
var copyCountedSets atomic.Int64

func copyArgsExecutor(t testing.TB) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		scalar Stamp
		scalar Loose
		scalar Bag
		enum Kind { A B }
		type Query {
			rows(first: Int, last: Int, name: String, on: Boolean, ratio: Float, id: ID, kind: Kind, stamp: Stamp, plain: Int! = 1): String!
			sum(first: Int, last: Int, name: String, on: Boolean, ratio: Float, id: ID, kind: Kind, stamp: Stamp, plain: Int! = 1): Int!
			hand(first: Int): Int!
			counted(first: Int): Int!
			wrapped(n: Int): Int!
			loose(v: Loose): Int!
			bag(b: Bag): Int!
		}`),
		Scalar("Bag", func(w *Writer, v copyBag) error { w.Int64(int64(len(v))); return nil },
			func(v any) (copyBag, error) {
				n, err := v.(json.Number).Int64()
				return copyBag{"n": int(n)}, err
			}),
		Args[copyCountedArgs](InputField("first", func(a *copyCountedArgs, v *int) {
			copyCountedSets.Add(1)
			a.First = v
		})),
		Enum("Kind", map[copyKind]string{0: "A", 1: "B"}),
		Scalar("ID", func(w *Writer, v copyID) error { w.String(string(v)); return nil },
			func(v any) (copyID, error) { return copyID(fmt.Sprint(v)), nil }),
		Scalar("Stamp", func(w *Writer, v copyStamp) error { w.Int64(v.At); return nil },
			func(v any) (copyStamp, error) {
				n, err := v.(json.Number).Int64()
				return copyStamp{At: n, Zone: [2]byte{'U', 'T'}}, err
			}),
		Scalar("Loose", func(w *Writer, v any) error { w.Null(); return nil },
			func(v any) (any, error) { return v, nil }),
		Args[copyHandArgs](InputField("first", func(a *copyHandArgs, v *int) { a.First, a.Also = v, v })),
		Query(
			// Every pointer is written through, and the answer is what was
			// there before the write.
			FieldArgs("rows", func(_ Root, a copyArgs) string {
				var b []byte
				if a.First != nil {
					b = fmt.Appendf(b, "first=%d ", *a.First)
					*a.First = -1
				}
				if a.Last != nil {
					b = fmt.Appendf(b, "last=%d ", *a.Last)
					*a.Last = -1
				}
				if a.Name != nil {
					b = fmt.Appendf(b, "name=%s ", *a.Name)
					*a.Name = "written"
				}
				if a.On != nil {
					b = fmt.Appendf(b, "on=%t ", *a.On)
					*a.On = !*a.On
				}
				if a.Ratio != nil {
					b = fmt.Appendf(b, "ratio=%g ", *a.Ratio)
					*a.Ratio = -1
				}
				if a.ID != nil {
					b = fmt.Appendf(b, "id=%s ", *a.ID)
					*a.ID = "written"
				}
				if a.Kind != nil {
					b = fmt.Appendf(b, "kind=%d ", *a.Kind)
					*a.Kind = 0
				}
				if a.Stamp != nil {
					b = fmt.Appendf(b, "stamp=%d%s ", a.Stamp.At, a.Stamp.Zone[:])
					a.Stamp.At, a.Stamp.Zone[0] = -1, 'x'
				}
				if a.hidden != nil {
					b = append(b, "hidden "...)
				}
				return fmt.Sprintf("%splain=%d", b, a.Plain)
			}),
			// Allocates nothing itself, so that what a request allocates for
			// it is what its arguments cost.
			FieldArgs("sum", func(_ Root, a copyArgs) int {
				n := a.Plain
				if a.First != nil {
					n += *a.First
				}
				if a.Last != nil {
					n += *a.Last
				}
				return n
			}),
			FieldArgs("hand", func(_ Root, a copyHandArgs) int {
				*a.First = -1
				return *a.Also
			}),
			FieldArgs("wrapped", func(_ Root, a copyWrappedArgs) int {
				p := a.N.Value()
				v := *p
				*p = -1
				return v
			}),
			FieldArgs("loose", func(_ Root, a copyAnyArgs) int { return 1 }),
			FieldArgs("counted", func(_ Root, a copyCountedArgs) int { return *a.First }),
			// A pointer to a map: copying what it points at copies a
			// reference to the one map.
			FieldArgs("bag", func(_ Root, a copyBagArgs) int {
				v := (*a.B)["n"]
				(*a.B)["n"] = -1
				return v
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

// The copy has to be as good as the decode it stands in for: each request's
// pointers are its own whatever they point at, the value the plan keeps is
// never the one a resolver wrote to, and everything else in the struct comes
// across. And it is taken only where it is known to be as good: a setter
// written by hand may have put one pointer in two fields, and that value is
// decoded again, as is a pointer the copy does not reach.
func TestALiteralArgumentCopiedForARequestIsItsOwn(t *testing.T) {
	type mode struct{ perRequest, copied bool }
	for _, tc := range []struct {
		name, query, want string
		mode              mode
	}{
		{"one pointer", `{ rows(first: 5) }`, `{"rows":"first=5 plain=1"}`, mode{true, true}},
		{"two pointers", `{ rows(first: 5, last: 6, plain: 2) }`, `{"rows":"first=5 last=6 plain=2"}`, mode{true, true}},
		{"every scalar kind", `{ rows(name: "n", on: true, ratio: 1.5, id: "i", kind: B, stamp: 7) }`,
			`{"rows":"name=n on=true ratio=1.5 id=i kind=1 stamp=7UT plain=1"}`, mode{true, true}},
		{"no pointer set", `{ rows(plain: 3) }`, `{"rows":"plain=3"}`, mode{false, false}},
		{"a setter written by hand", `{ counted(first: 5) }`, `{"counted":5}`, mode{true, true}},
		{"one pointer a setter put in two fields", `{ hand(first: 5) }`, `{"hand":-1}`, mode{true, false}},
		{"an Omittable around a pointer", `{ wrapped(n: 5) }`, `{"wrapped":5}`, mode{true, false}},
		{"a value behind an interface", `{ loose(v: 5) }`, `{"loose":1}`, mode{true, false}},
		{"a pointer to what a copy would still share", `{ bag(b: 5) }`, `{"bag":5}`, mode{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := copyArgsExecutor(t)
			oc, errs := e.prepareOperation(&Request{Query: tc.query}, false)
			if errs != nil {
				t.Fatalf("%v", errs[0])
			}
			if f := oc.plan.sel.fields[0]; (mode{f.dynamicArgs, f.freshArgs}) != tc.mode {
				t.Fatalf("per request = %t, by copying = %t; want %t and %t", f.dynamicArgs, f.freshArgs, tc.mode.perRequest, tc.mode.copied)
			}
			for range 3 {
				expectData(t, run(t, e, tc.query, ""), tc.want)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					for range 50 {
						resp := run(t, e, tc.query, "")
						if got := string(resp.Data); got != tc.want || len(resp.Errors) > 0 {
							t.Errorf("data = %s, want %s (%s)", got, tc.want, errorsJSON(resp.Errors))
							return
						}
						resp.Release()
					}
				})
			}
			wg.Wait()
		})
	}
}

// A setter written by hand runs when the plan is compiled and not again for
// a literal: the copy stands in for it, as the shared value did before a
// request was given arguments of its own. Running it for every request cost
// such a binding nine allocations where a derived one pays one.
func TestASetterWrittenByHandRunsOnceForAPlannedLiteral(t *testing.T) {
	e := copyArgsExecutor(t)
	const query = `{ counted(first: 5) }`
	expectData(t, run(t, e, query, ""), `{"counted":5}`)
	before := copyCountedSets.Load()
	for range 3 {
		expectData(t, run(t, e, query, ""), `{"counted":5}`)
	}
	if got := copyCountedSets.Load() - before; got != 0 {
		t.Fatalf("the setter ran %d times for three requests of a planned query, want 0", got)
	}
}

// The copy is one allocation: the struct and what its pointers point at are
// made together. Decoding again was five for first: 5 -- the argument map,
// the number, the struct, the int -- on the commonest argument shape there
// is. Held against the same field with no pointer set, which shares the
// plan's value and allocates nothing for its arguments.
func TestCopyingLiteralArgumentsIsOneAllocation(t *testing.T) {
	if raceEnabled {
		t.Skip("-race changes allocation counts")
	}
	e := copyArgsExecutor(t)
	allocs := func(query string) float64 {
		req := &Request{Query: query}
		if resp := e.Execute(t.Context(), req); len(resp.Errors) > 0 {
			t.Fatalf("%s: %s", query, errorsJSON(resp.Errors))
		}
		return testing.AllocsPerRun(200, func() { e.Execute(t.Context(), req).Release() })
	}
	shared := allocs(`{ sum(plain: 3) }`)
	for _, query := range []string{`{ sum(first: 5) }`, `{ sum(first: 5, last: 6) }`} {
		if got := allocs(query) - shared; got != 1 {
			t.Errorf("%s: %v allocations for its arguments over the shared decode, want one", query, got)
		}
	}
}
