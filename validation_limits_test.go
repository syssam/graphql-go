package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/lexer"
	"github.com/vektah/gqlparser/v2/parser"
)

func countTokens(q string) int {
	l := lexer.New(&ast.Source{Input: q})
	n := 0
	for {
		tok, err := l.ReadToken()
		if err != nil || tok.Kind == lexer.EOF {
			return n
		}
		n++
	}
}

func repeated(n int, f func(i int) string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(f(i))
	}
	return b.String()
}

// validationAttacks are the document shapes an external review found
// super-linear in gqlparser's default rules, each against the fixture. The
// comment is what each cost before the limits, measured by that review.
var validationAttacks = map[string]func(n int) string{
	// OverlappingFieldsCanBeMerged compares every pair: 10 KB, 1.2 s, 1.25 GB.
	"repeated field": func(n int) string {
		return "{" + repeated(n, func(int) string { return "me{id} " }) + "}"
	},
	// Every pair conflicts, and an error is built for each: 27 KB, 3.3 s, 2 GB.
	"aliases with differing arguments": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("a:echo(v:%d) ", i) }) + "}"
	},
	"same alias, same field": func(n int) string {
		return "{" + repeated(n, func(int) string { return "a:echo(v:1) " }) + "}"
	},
	// Each fragment spreads the next twice: 51 KB, 5.8 s.
	"fragment DAG": func(n int) string {
		q := "{me{...F0}}" + repeated(n, func(i int) string { return fmt.Sprintf(" fragment F%d on User{id ...F%d ...F%d}", i, i+1, i+1) })
		return q + fmt.Sprintf(" fragment F%d on User{id}", n)
	},
	"fragment DAG under fields": func(n int) string {
		q := "{me{...F0}}" + repeated(n, func(i int) string {
			return fmt.Sprintf(" fragment F%d on User{friends{...F%d} bestFriend{...F%d}}", i, i+1, i+1)
		})
		return q + fmt.Sprintf(" fragment F%d on User{id}", n)
	},
	"many fragments spread once": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("...F%d ", i) }) + "}" +
			repeated(n, func(i int) string { return fmt.Sprintf("fragment F%d on Query{me{id}} ", i) })
	},
	"inline fragments": func(n int) string {
		return "{" + repeated(n, func(int) string { return "...on Query{me{id}} " }) + "}"
	},
	// Collected again at every level: 200 KB, 26.7 s.
	"nested inline fragments": func(n int) string {
		return "{" + strings.Repeat("...{", n) + "__typename" + strings.Repeat("}", n) + "}"
	},
	// ValuesOfCorrectType stringifies every level: 1 MiB, 3 min 17 s.
	"nested list literal": func(n int) string {
		return "{__typename@skip(if:" + strings.Repeat("[", n) + strings.Repeat("]", n) + ")}"
	},
	"many variables": func(n int) string {
		return "query(" + repeated(n, func(i int) string { return fmt.Sprintf("$v%d:Int ", i) }) + "){" +
			repeated(n, func(i int) string { return fmt.Sprintf("a%d:echo(v:$v%d) ", i, i) }) + "}"
	},
}

// largestAccepted is the biggest document of a shape the default limits let
// through to validation.
func largestAccepted(gen func(int) string) string {
	fits := func(n int) bool {
		q := gen(n)
		if countTokens(q) > defaultMaxTokens {
			return false
		}
		doc := mustParse(q)
		return measureDocument(doc, 0).nesting <= defaultMaxNesting && fragmentLookups(doc, fragmentLookupBudget) <= fragmentLookupBudget
	}
	lo, hi := 1, 2
	for fits(hi) {
		lo, hi = hi, hi*2
	}
	for lo+1 < hi {
		if m := (lo + hi) / 2; fits(m) {
			lo = m
		} else {
			hi = m
		}
	}
	return gen(lo)
}

// Parsing and validation run before every limit a deployment sets, so the
// default document limits are what bound them. At the largest document they
// accept, every shape above validates in milliseconds; before, the same
// shapes took seconds at a few kilobytes and minutes at a megabyte.
// validationTime is the fastest of three validations of q, each on a fresh
// executor because a validated document is cached: a bound on the fastest
// fails for the cost, not for a busy runner.
func validationTime(t *testing.T, q string, opts ...ExecutorOption) (time.Duration, []*Error) {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	var errs []*Error
	for range 3 {
		_, e := newFixtureExecutor(t, opts...)
		start := time.Now()
		_, errs = e.document(q)
		best = min(best, time.Since(start))
	}
	return best, errs
}

// validationBound is 500 ms, five times that under -race. Unbounded, the
// shapes it guards took seconds to minutes uninstrumented.
func validationBound() time.Duration {
	if raceEnabled {
		return 2500 * time.Millisecond
	}
	return 500 * time.Millisecond
}

func TestValidationIsBoundedByTheDefaultLimits(t *testing.T) {
	for name, gen := range validationAttacks {
		t.Run(name, func(t *testing.T) {
			q := largestAccepted(gen)
			d, errs := validationTime(t, q)
			t.Logf("%d bytes, %d tokens: %v, %d errors", len(q), countTokens(q), d.Round(time.Microsecond), len(errs))
			if d > validationBound() {
				t.Errorf("validating %d bytes took %v", len(q), d)
			}
			for _, err := range errs {
				if strings.Contains(err.Message, "exceeded token limit") || strings.Contains(err.Message, "nesting exceeds") || strings.Contains(err.Message, "too expensive") {
					t.Fatalf("largestAccepted produced a refused document: %s", err.Message)
				}
			}
		})
	}
}

// A document past either limit is refused before validation, and quickly:
// the parser stops at the token limit, and the nesting walk one level past
// the nesting limit.
func TestDocumentLimitsRefuseEarly(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for _, tc := range []struct {
		name, query, want string
	}{
		{"tokens", "{" + strings.Repeat("me{id} ", 1<<17) + "}", "exceeded token limit of 15000"},
		{"selection nesting", "{me" + strings.Repeat("{bestFriend", 101) + "{id}" + strings.Repeat("}", 101) + "}", "document nesting exceeds the limit of 100"},
		{"inline fragment nesting", "{" + strings.Repeat("...{", 101) + "__typename" + strings.Repeat("}", 101) + "}", "document nesting exceeds the limit of 100"},
		{"value nesting", "{__typename@skip(if:" + strings.Repeat("[", 102) + strings.Repeat("]", 102) + ")}", "document nesting exceeds the limit of 100"},
		{"object value nesting", "{users(filter:" + strings.Repeat("{name:", 101) + `"x"` + strings.Repeat("}", 101) + "){id}}", "document nesting exceeds the limit of 100"},
		{"fragment DAG", validationAttacks["fragment DAG"](1200), "document is too expensive to validate: its fragments spread one another too many times"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			resp := run(t, e, tc.query, "")
			d := time.Since(start)
			code := CodeParseFailed
			if tc.name == "fragment DAG" {
				code = CodeValidationFailed
			}
			if len(resp.Errors) != 1 || resp.Errors[0].Message != tc.want || resp.Errors[0].Extensions["code"] != code {
				t.Fatalf("want one %s error %q, got %s", code, tc.want, errorsJSON(resp.Errors))
			}
			if d > 500*time.Millisecond {
				t.Errorf("refusing took %v", d)
			}
		})
	}

	// And each limit can be lifted, or moved.
	_, open := newFixtureExecutor(t, WithMaxTokens(0), WithMaxNesting(0))
	if resp := run(t, open, "{me"+strings.Repeat("{bestFriend", 101)+"{id}"+strings.Repeat("}", 101)+"}", ""); len(resp.Errors) > 0 {
		t.Fatalf("WithMaxNesting(0) still refused: %s", errorsJSON(resp.Errors))
	}
	_, tight := newFixtureExecutor(t, WithMaxTokens(10), WithMaxNesting(2))
	if resp := run(t, tight, "{me{bestFriend{id}}}", ""); len(resp.Errors) != 1 || resp.Errors[0].Message != "document nesting exceeds the limit of 2" {
		t.Fatalf("WithMaxNesting(2): %s", errorsJSON(resp.Errors))
	}
	if resp := run(t, tight, "{me{id} a:me{id}}", ""); len(resp.Errors) != 1 || resp.Errors[0].Message != "exceeded token limit of 10" {
		t.Fatalf("WithMaxTokens(10): %s", errorsJSON(resp.Errors))
	}
}

func mustParse(q string) *ast.QueryDocument {
	doc, err := parser.ParseQuery(&ast.Source{Input: q})
	if err != nil {
		panic(err)
	}
	return doc
}

// The fragment budget is for DAGs; what a client compiler emits is a tree.
// Relay gives every component its own fragment and spreads its children's,
// so a large screen is hundreds of fragments a few levels deep, each reached
// from one parent. 511 of them, nine deep, are accepted with room to spare.
func TestFragmentBudgetAcceptsAClientShapedDocument(t *testing.T) {
	var b strings.Builder
	b.WriteString("{ me { ...F1 } }\n")
	const n = 511
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "fragment F%d on User { id name", i)
		for _, c := range []int{2 * i, 2*i + 1} {
			if c <= n {
				fmt.Fprintf(&b, " bestFriend { ...F%d }", c)
			}
		}
		b.WriteString(" }\n")
	}
	q := b.String()
	doc := mustParse(q)
	lookups := fragmentLookups(doc, fragmentLookupBudget)
	if lookups*10 > fragmentLookupBudget {
		t.Fatalf("a %d-fragment tree makes %d lookups, within 10x of the budget %d", n, lookups, fragmentLookupBudget)
	}
	_, e := newFixtureExecutor(t, WithMaxTokens(0))
	if _, errs := e.document(q); errs != nil {
		t.Fatalf("%s", errorsJSON(errs))
	}
	t.Logf("%d fragments: %d lookups of a budget of %d", n, lookups, fragmentLookupBudget)
}

// A recursive input is every ent- or velox-generated WhereInput (not, and,
// or), and ValuesOfCorrectType does work at every level of a literal
// proportional to what is below it: 60 KB of {not:{not:...}} was 13 s and
// 17 GB. The nesting limit caps each chain, and the token limit their number.
func TestRecursiveInputLiteralIsBounded(t *testing.T) {
	type where struct {
		Name *string
		Not  *where
		And  []*where
	}
	type qArgs struct{ F *where }
	s, err := NewSchema(SDL(`
		input Where { name: String, not: Where, and: [Where!] }
		type Query { q(f: Where): String }
	`),
		Input[where]("Where"),
		Args[qArgs](),
		Query(ResolveArgs("q", func(context.Context, Root, qArgs) (string, error) { return "", nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	chain := func(depth int) string {
		return "q(f:" + strings.Repeat("{not:", depth) + `{name:"x"}` + strings.Repeat("}", depth) + ")"
	}
	// As many of the deepest accepted chains as fit in the token limit.
	var b strings.Builder
	b.WriteString("{")
	for i := 0; countTokens(b.String()) < defaultMaxTokens-500; i++ {
		fmt.Fprintf(&b, " a%d:%s", i, chain(defaultMaxNesting-1))
	}
	b.WriteString("}")
	q := b.String()
	best := time.Duration(1<<63 - 1)
	for range 3 {
		e := NewExecutor(s)
		start := time.Now()
		if _, errs := e.document(q); errs != nil {
			t.Fatalf("%s", errorsJSON(errs))
		}
		best = min(best, time.Since(start))
	}
	d := best
	t.Logf("%d bytes, %d tokens: %v", len(q), countTokens(q), d.Round(time.Microsecond))
	if d > validationBound() {
		t.Errorf("validating took %v", d)
	}
}

// Below overlapExactMaxSelections gqlparser's own rule runs, for its exact
// messages, so its worst case at that size has to be cheap too.
func TestExactOverlapRuleIsCheapBelowItsThreshold(t *testing.T) {
	for name, gen := range validationAttacks {
		// A shape whose selection count does not grow with n, a literal
		// nested in one field, is not this rule's to bound.
		if measureDocument(mustParse(gen(2)), 0).selections == measureDocument(mustParse(gen(4)), 0).selections {
			continue
		}
		lo, hi := 1, 2
		for measureDocument(mustParse(gen(hi)), 0).selections <= overlapExactMaxSelections {
			lo, hi = hi, hi*2
		}
		for lo+1 < hi {
			if m := (lo + hi) / 2; measureDocument(mustParse(gen(m)), 0).selections <= overlapExactMaxSelections {
				lo = m
			} else {
				hi = m
			}
		}
		q := gen(lo)
		d, errs := validationTime(t, q)
		t.Logf("%-34s %d selections: %v, %d errors", name, measureDocument(mustParse(q), 0).selections, d.Round(time.Microsecond), len(errs))
		// About 35 ms at worst uninstrumented; at a few thousand selections
		// these are seconds again.
		if d > validationBound() {
			t.Errorf("%s: %v at the threshold", name, d)
		}
	}
}
