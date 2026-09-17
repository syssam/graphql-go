package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/internal/jsonw"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

func compileFixture(t *testing.T, e *Executor, query string, cond map[string]bool) *plan {
	t.Helper()
	doc, err := parser.ParseQuery(astSource(query))
	if err != nil {
		t.Fatal(err)
	}
	if errs := validator.ValidateWithRules(e.schema.ast, doc, rules.NewDefaultRules()); len(errs) > 0 {
		t.Fatal(errs)
	}
	p, perrs := compilePlan(e.schema, e, doc, doc.Operations[0], cond)
	if perrs != nil {
		t.Fatal(perrs[0])
	}
	// compilePlan no longer fills p.complexity/p.depth itself: planFor's
	// compile helper supplies them from operationMetrics before compiling, so
	// a test bypassing planFor has to do the same to see the numbers a real
	// request would.
	m := operationMetrics(e.schema, doc, doc.Operations[0], cond)
	p.complexity = m.complexity
	p.depth = m.depth
	return p
}

func fieldNames(fields []*planField) string {
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.alias
	}
	return strings.Join(names, ",")
}

func TestPlanMergesResponseKeys(t *testing.T) {
	_, e := newFixtureExecutor(t)
	p := compileFixture(t, e, `{ me { id ...A friends { id } } } fragment A on User { friends { name } id }`, nil)
	me := p.sel.fields[0]
	if got := fieldNames(p.sel.fields); got != "me" {
		t.Fatalf("root fields = %s", got)
	}
	if got := fieldNames(me.sub.fields); got != "id,friends" {
		t.Fatalf("me fields = %s", got)
	}
	friends := me.sub.fields[1]
	if got := fieldNames(friends.sub.fields); got != "name,id" {
		t.Fatalf("merged friends sub-selection = %s", got)
	}
	if string(friends.key) != string(jsonw.EncodeKey("friends")) {
		t.Fatalf("key bytes = %s", friends.key)
	}
}

func TestPlanAbstractByType(t *testing.T) {
	_, e := newFixtureExecutor(t)
	p := compileFixture(t, e, `{ me { pet { name ... on Dog { barks } ... on Cat { lives } } } }`, nil)
	pet := p.sel.fields[0].sub.fields[0]
	if pet.abstract == nil || pet.sub.byType == nil {
		t.Fatal("pet should compile as an abstract selection")
	}
	if got := fieldNames(pet.sub.byType["Dog"].fields); got != "name,barks" {
		t.Fatalf("Dog fields = %s", got)
	}
	if got := fieldNames(pet.sub.byType["Cat"].fields); got != "name,lives" {
		t.Fatalf("Cat fields = %s", got)
	}
}

func TestPlanConditions(t *testing.T) {
	_, e := newFixtureExecutor(t)
	q := `query($a: Boolean!, $b: Boolean!) { me { id @include(if: $a) name @skip(if: $b) nick @skip(if: true) tags @include(if: true) } }`
	doc, _ := parser.ParseQuery(astSource(q))
	if got := strings.Join(condVariables(doc), ","); got != "a,b" {
		t.Fatalf("condVariables = %s", got)
	}
	p := compileFixture(t, e, q, map[string]bool{"a": true, "b": false})
	if got := fieldNames(p.sel.fields[0].sub.fields); got != "id,name,tags" {
		t.Fatalf("fields = %s", got)
	}
	p = compileFixture(t, e, q, map[string]bool{"a": false, "b": true})
	if got := fieldNames(p.sel.fields[0].sub.fields); got != "tags" {
		t.Fatalf("fields = %s", got)
	}
	key, _ := variantKey([]string{"a", "b"}, map[string]any{"a": true, "b": true})
	if key != 3 {
		t.Fatalf("variant key = %d", key)
	}
}

func TestPlanArgumentPreDecoding(t *testing.T) {
	_, e := newFixtureExecutor(t)
	p := compileFixture(t, e, `query($n: String) { a: users(filter: {limit: 1}) { id } b: users(filter: {name: $n}) { id } c: users { id } }`, nil)
	a, b, c := p.sel.fields[0], p.sel.fields[1], p.sel.fields[2]
	if a.dynamicArgs || a.args == nil || *a.args.(*usersArgs).Filter.Limit != 1 {
		t.Fatalf("literal arguments must be pre-decoded: %+v", a.args)
	}
	if !b.dynamicArgs || b.args != nil {
		t.Fatal("arguments referencing variables must be dynamic")
	}
	if c.dynamicArgs || c.args == nil || c.args.(*usersArgs).Filter != nil {
		t.Fatalf("absent optional argument must decode to zero value: %+v", c.args)
	}
	if p.complexity != 6 {
		t.Fatalf("complexity = %d, want 6", p.complexity)
	}
	if p.depth != 2 {
		t.Fatalf("depth = %d, want 2", p.depth)
	}
}

func TestPlanSchedulability(t *testing.T) {
	_, e := newFixtureExecutor(t)
	p := compileFixture(t, e, `{ me { id name } users { id friends { id } } }`, nil)
	me, users := p.sel.fields[0], p.sel.fields[1]
	if me.sub.directSchedulable != 0 || me.sub.deepSchedulable {
		t.Fatal("pure-only selection must not be schedulable")
	}
	if users.sub.directSchedulable != 1 || !users.sub.deepSchedulable {
		t.Fatalf("users selection: direct=%d deep=%v", users.sub.directSchedulable, users.sub.deepSchedulable)
	}
	if p.sel.directSchedulable != 2 {
		t.Fatalf("root direct schedulable = %d", p.sel.directSchedulable)
	}
}

func astSource(q string) *ast.Source { return &ast.Source{Input: q} }

func TestPlanCacheHitAndEviction(t *testing.T) {
	c := newPlanCache(2, 0)
	a, b, d := &docEntry{query: "a"}, &docEntry{query: "b"}, &docEntry{query: "d"}
	c.put(a)
	c.put(b)
	if c.get("a") != a || c.get("b") != b {
		t.Fatal("expected hits")
	}
	c.get("a") // a becomes most recent; b is now the eviction candidate.
	c.put(d)
	if c.get("b") != nil {
		t.Fatal("b should have been evicted")
	}
	if c.get("a") != a || c.get("d") != d || c.len() != 2 {
		t.Fatal("unexpected cache state")
	}
}

func TestPlanCacheCollision(t *testing.T) {
	c := newPlanCache(4, 0)
	c.hash = func(string) uint64 { return 42 }
	a := &docEntry{query: "a"}
	c.put(a)
	if c.get("b") != nil {
		t.Fatal("a colliding query must miss, never return a foreign document")
	}
	if c.get("a") != a {
		t.Fatal("original entry must still hit")
	}
	b := &docEntry{query: "b"}
	c.put(b)
	if c.get("b") != b || c.get("a") != nil || c.len() != 1 {
		t.Fatal("colliding put must replace the slot")
	}
}

func TestPlanCacheDisabled(t *testing.T) {
	c := newPlanCache(0, 0)
	c.put(&docEntry{query: "a"})
	if c.get("a") != nil || c.len() != 0 {
		t.Fatal("zero-size cache must not store")
	}
	var nilCache *planCache
	if nilCache.get("a") != nil {
		t.Fatal("nil cache must miss")
	}
}

// condQuery builds a document referencing n distinct Boolean variables from
// @include, which is what docEntry.condVars counts.
func condQuery(n int) (query, vars string) {
	var decl, sel, vs strings.Builder
	for i := range n {
		if i > 0 {
			decl.WriteString(", ")
			vs.WriteString(",")
		}
		fmt.Fprintf(&decl, "$v%d: Boolean!", i)
		fmt.Fprintf(&sel, " s%d: strings @include(if: $v%d)", i, i)
		fmt.Fprintf(&vs, "\"v%d\":true", i)
	}
	return fmt.Sprintf("query Q(%s) {%s }", decl.String(), sel.String()), "{" + vs.String() + "}"
}

func statsFor(t *testing.T, condVars int) []OperationStats {
	t.Helper()
	var seen []OperationStats
	_, e := newFixtureExecutor(t, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			seen = append(seen, oc.Stats)
			return next(ctx, oc)
		})))
	query, vars := condQuery(condVars)
	for range 2 {
		if resp := run(t, e, query, vars); len(resp.Errors) > 0 {
			t.Fatalf("query errors: %v", resp.Errors)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("operations seen = %d, want 2", len(seen))
	}
	return seen
}

// Over the variant cap the plan is recompiled on every request. CacheHit
// cannot say so on its own — it is also false on the first request for a
// perfectly cacheable document — so the reason is reported separately.
func TestPlanOverCondVarCapReportsUncacheable(t *testing.T) {
	seen := statsFor(t, maxCondVars+1)
	if !seen[1].PlanUncacheable {
		t.Fatal("document over maxCondVars is recompiled every request but does not report it")
	}
	if seen[1].CacheHit {
		t.Fatal("CacheHit must stay false when no plan was cached")
	}
}

// The control: at the cap the plan caches and nothing is flagged.
func TestPlanAtCondVarCapIsCached(t *testing.T) {
	seen := statsFor(t, maxCondVars)
	if seen[1].PlanUncacheable {
		t.Fatal("a cacheable document was reported uncacheable")
	}
	if !seen[1].CacheHit {
		t.Fatal("second identical request missed the plan cache")
	}
}
