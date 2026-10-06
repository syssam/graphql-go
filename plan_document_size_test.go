package graphql

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// documentShapes are documents that differ in what they are made of, so an
// estimate that is right for one kind of node and wrong for another shows.
// Each takes an index that makes its text distinct.
var documentShapes = []struct {
	name string
	gen  func(i int) string
}{
	{"one small field", func(i int) string { return fmt.Sprintf(`{ q%d: ctxPath }`, i) }},
	{"an ordinary query", ordinaryDocument},
	{"aliases", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "{ q%d: me {", i)
		for j := range 400 {
			fmt.Fprintf(&q, " a%d: id", j)
		}
		q.WriteString(" } }")
		return q.String()
	}},
	{"nested objects", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "{ q%d: me {", i)
		for range 40 {
			q.WriteString(" id name bestFriend {")
		}
		q.WriteString(" id")
		q.WriteString(strings.Repeat(" }", 40))
		q.WriteString(" } }")
		return q.String()
	}},
	{"arguments", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 100 {
			fmt.Fprintf(&q, ` e%d_%d: echo(v: %d, s: "s", f: 1.5, b: true, list: [1, 2, 3]) u%d: users(filter: {name: "n", limit: 5}) { id }`, i, j, j, j)
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"variables and directives", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "query Q%d(", i)
		for j := range 50 {
			fmt.Fprintf(&q, "$b%d: Boolean = false $l%d: [Int!] = [1, 2] ", j, j)
		}
		q.WriteString(") {")
		for j := range 50 {
			fmt.Fprintf(&q, " s%d: ctxPath @skip(if: $b%d) e%d: echo(list: $l%d) @include(if: true)", j, j, j, j)
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"fragments", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "query Q%d { me { ...F0 } node(id: \"1\") {", i)
		for j := range 100 {
			fmt.Fprintf(&q, " ... on User { ...F%d } ... on Post { t%d: title }", j, j)
		}
		q.WriteString(" } }")
		for j := range 100 {
			fmt.Fprintf(&q, " fragment F%d on User { n%d: name }", j, j)
		}
		return q.String()
	}},
	{"comments", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "{ q%d: me {", i)
		for j := range 100 {
			q.WriteString(strings.Repeat("\n#", 20))
			fmt.Fprintf(&q, "\n a%d: id", j)
		}
		q.WriteString(" } }")
		return q.String()
	}},
	{"lists", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 20 {
			fmt.Fprintf(&q, " e%d_%d: echo(list: [%s 0])", i, j, strings.Repeat("1, ", 100))
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"input objects", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 200 {
			fmt.Fprintf(&q, ` u%d_%d: users(filter: {name: "n", limit: 5}) { id }`, i, j)
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"directives", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 100 {
			fmt.Fprintf(&q, " s%d_%d: ctxPath @skip(if: false) @include(if: true)", i, j)
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"one large fragment", func(i int) string {
		var q strings.Builder
		fmt.Fprintf(&q, "query Q%d { me { ...F } } fragment F on User {", i)
		for j := range 400 {
			fmt.Fprintf(&q, " a%d: id", j)
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"block strings", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 4 {
			fmt.Fprintf(&q, ` e%d_%d: echo(s: """%s""")`, i, j, strings.Repeat("block ", 700))
		}
		q.WriteString(" }")
		return q.String()
	}},
	{"strings", func(i int) string {
		var q strings.Builder
		q.WriteString("{")
		for j := range 50 {
			fmt.Fprintf(&q, ` e%d_%d: echo(s: """%s""") x%d: echo(s: "a\n%s")`, i, j, strings.Repeat("block ", 30), j, strings.Repeat("escaped ", 20))
		}
		q.WriteString(" }")
		return q.String()
	}},
}

// ordinaryDocument is what a client's screen sends: a named operation with a
// couple of variables, a few objects a few levels deep, an argument here and
// there and a fragment, in about 1.2 KB.
func ordinaryDocument(i int) string {
	return fmt.Sprintf(`query Screen%d($id: ID!, $term: String!, $withPet: Boolean = true) {
  me {
    id
    name
    nick
    role
    tags
    pet @include(if: $withPet) { name ... on Dog { barks } ... on Cat { lives } }
    bestFriend { ...Person friends { ...Person } }
    friends { ...Person bestFriend { id name } }
  }
  user(id: $id) {
    ...Person
    scores
    friends { id name role pet { name } }
  }
  users(filter: {name: "a", limit: 20}) { ...Person }
  search(term: $term) {
    __typename
    ... on User { ...Person }
    ... on Post { id title author { ...Person } }
  }
  node(id: $id) {
    id
    ... on User { name nick tags }
    ... on Post { title author { id name } }
  }
  usersSeq { id name role tags }
  maybeUsers { ...Person friends { id name } }
  first: users(filter: {limit: 1}) { id name nick bestFriend { id name } }
  searchSeq(term: $term) { ... on Node { id } ... on Post { title } }
  strings
  nullableStrings
  selection
  echo(v: 1, s: "screen", list: [1, 2, 3])
  choose(c: {name: "screen"})
}

fragment Person on User {
  id
  name
  nick
  role
  tags
  pet { name ... on Dog { barks } ... on Cat { lives } }
  bestFriend { id name nick }
}
`, i)
}

// The parsed document is most of what a cached document holds and the budget
// did not count it. The charge is an estimate from one walk of the document,
// and what it has to be is the right size whatever the document is made of:
// within a third of the heap it is measured to hold, and so never short of
// it by half.
func TestADocumentsChargeIsNearWhatItHolds(t *testing.T) {
	if raceEnabled {
		t.Skip("-race changes what an allocation costs")
	}
	for _, shape := range documentShapes {
		t.Run(shape.name, func(t *testing.T) {
			const docs = 400
			_, e := newFixtureExecutor(t, WithPlanCache(docs), WithPlanCacheBytes(0))
			before := heapInUse()
			var text int64
			for i := range docs {
				q := shape.gen(i)
				text += int64(len(q))
				if _, errs := e.document(q); errs != nil {
					t.Fatalf("document %d: %v", i, errs[0])
				}
			}
			held := heapInUse() - before
			n, charged := e.cache.occupancy()
			if n != docs {
				t.Fatalf("%d of %d documents are cached", n, docs)
			}
			t.Logf("a document of %d bytes is charged %d and holds %d: %.2f of it, and %.1f times its text",
				text/docs, charged/docs, held/docs, float64(charged)/float64(held), float64(held)/float64(text))
			if charged < held*3/4 || charged > held*4/3 {
				t.Fatalf("%d documents are charged %d bytes and hold %d", docs, charged, held)
			}
			runtime.KeepAlive(e)
		})
	}
}

// largeDocument is a small operation beside a large one in the same text, to
// be run by the small one's name: about 49 KB, under every document limit.
func largeDocument(i int) (query, operation string) {
	var q strings.Builder
	operation = fmt.Sprintf("Small%d", i)
	fmt.Fprintf(&q, "query %s { ctxPath } query Large { me {", operation)
	for j := range 4900 {
		fmt.Fprintf(&q, " a%d: id", j)
	}
	q.WriteString(" } }")
	return q.String(), operation
}

// This is the case the budget is for and did not bound: distinct documents,
// each large, each running an operation too small for any limit to notice.
// Counted as text, 1 100 of them at 54 KB held 265 MB under a budget of
// 16 MiB. Counted as what they hold, the cache keeps what the budget has room
// for and the heap says the same.
func TestThePlanCacheBoundsWhatLargeDocumentsHold(t *testing.T) {
	const docs, budget = 120, 4 << 20
	_, e := newFixtureExecutor(t, WithPlanCacheBytes(budget))
	before := heapInUse()
	var text, offered int64
	for i := range docs {
		q, name := largeDocument(i)
		text += int64(len(q))
		resp := e.Execute(t.Context(), &Request{Query: q, OperationName: name})
		if len(resp.Errors) > 0 {
			t.Fatalf("document %d: %s", i, errorsJSON(resp.Errors))
		}
		resp.Release()
		if entry := e.cache.get(q); entry != nil {
			offered += documentBytes(entry)
		}
	}
	// Without pressure on both counts the bound below is met by doing nothing.
	if text <= budget || offered <= 8*budget {
		t.Fatalf("%d bytes of text holding %d were offered to a budget of %d; the test applies no pressure", text, offered, budget)
	}
	n, used := e.cache.occupancy()
	if n == 0 || used > budget || used != cachedBytes(e.cache) {
		t.Fatalf("cache holds %d documents charged %d bytes (%d by its contents) against a budget of %d", n, used, cachedBytes(e.cache), budget)
	}
	if n > 8 {
		t.Fatalf("%d documents of %d KB of text each are cached under a budget of %d KB: they are counted as their text", n, text/docs>>10, budget>>10)
	}
	if !raceEnabled {
		// The estimate is within a third of the heap, and so is the bound.
		if grew := heapInUse() - before; grew > budget*4/3 {
			t.Fatalf("the heap grew by %d KB under a budget of %d KB", grew>>10, budget>>10)
		}
	}
	runtime.KeepAlive(e)
}

// The default is sized to the default entry count: with documents counted as
// what they hold, 64 MiB is the 1 024 ordinary documents the cache allows,
// plans included, where 16 MiB would be a third of them.
func TestTheDefaultPlanCacheHoldsEveryOrdinaryDocument(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const docs = 1024
	if e.cache.size != docs {
		t.Fatalf("the default entry count is %d; this test is written for %d", e.cache.size, docs)
	}
	var first string
	var text int64
	for i := range docs {
		q := ordinaryDocument(i)
		if i == 0 {
			first = q
		}
		text += int64(len(q))
		resp := run(t, e, q, `{"id":"1","term":"a"}`)
		if len(resp.Errors) > 0 {
			t.Fatalf("document %d: %s", i, errorsJSON(resp.Errors))
		}
		resp.Release()
	}
	if kb := text / docs; kb < 1100 || kb > 1300 {
		t.Fatalf("the ordinary document is %d bytes; the default is described for one of 1.2 KB", kb)
	}
	n, used := e.cache.occupancy()
	if entry := e.cache.get(ordinaryDocument(docs - 1)); entry != nil {
		parsed := documentBytes(entry) - int64(len(entry.query))
		t.Logf("%d documents of %d bytes are charged %d each -- %d parsed and a plan of %d -- and %.1f MiB in all",
			n, text/docs, used/int64(n), parsed, used/int64(n)-documentBytes(entry), float64(used)/(1<<20))
	}
	if n != docs {
		t.Fatalf("%d of %d ordinary documents fit the default budget of %d MiB", n, docs, e.cache.maxBytes>>20)
	}
	if used != cachedBytes(e.cache) {
		t.Fatalf("cache counts %d bytes and holds %d", used, cachedBytes(e.cache))
	}
	// The first is the least recently used, so its plan is the first to go.
	if entry := e.cache.get(first); entry == nil || storedPlans(entry) != 1 {
		t.Fatal("the first document's plan was not kept")
	}
	if used <= 32<<20 {
		t.Fatalf("%d documents are charged %d MiB; the default has more room than this test shows it needs", docs, used>>20)
	}
}
