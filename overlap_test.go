package graphql

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/core"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

// validUnder runs one rule alone, so the two implementations are compared
// on the only thing that decides whether a request runs.
func validUnder(s *ast.Schema, query string, r core.Rule) (bool, error) {
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return false, err
	}
	return len(validator.ValidateWithRules(s, doc, rules.NewRules(r))) == 0, nil
}

var gqlparserOverlap = rules.OverlappingFieldsCanBeMergedRule

// gqlparserSpecDir is where gqlparser keeps graphql-js's validation spec,
// ported to YAML: its OverlappingFieldsCanBeMerged cases are the oracle.
func gqlparserSpecDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/vektah/gqlparser/v2").Output()
	if err != nil {
		t.Skipf("locating gqlparser's module: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "validator", "imported", "spec")
}

type overlapSpecCase struct {
	name   string
	schema int
	query  string
}

// readOverlapSpec reads the few shapes of YAML these two files use, so the
// root module's tests need no YAML dependency: a list of `- |-` blocks for
// the schemas, and `- name:` entries with a `query: |2-` block for the cases.
func readOverlapSpec(t *testing.T, dir string) ([]*ast.Schema, []overlapSpecCase) {
	t.Helper()
	read := func(name string) []string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(string(b), "\n")
	}
	var schemas []*ast.Schema
	var cur []string
	flush := func() {
		if cur != nil {
			s, err := gqlparser.LoadSchema(&ast.Source{Input: strings.Join(cur, "\n")})
			if err != nil {
				t.Fatalf("schemas.yml[%d]: %v\n%s", len(schemas), err, strings.Join(cur, "\n"))
			}
			schemas = append(schemas, s)
		}
		cur = nil
	}
	for _, l := range read("schemas.yml") {
		switch {
		case l == "- |-":
			flush()
			cur = []string{}
		case strings.HasPrefix(l, "- "):
			// An inline item, such as - '', which no case of this rule uses;
			// kept as a placeholder so the indices stay right.
			flush()
			schemas = append(schemas, nil)
		case cur != nil:
			cur = append(cur, strings.TrimPrefix(l, "  "))
		}
	}
	flush()

	var cases []overlapSpecCase
	var c *overlapSpecCase
	inQuery := false
	for _, l := range read("OverlappingFieldsCanBeMergedRule.spec.yml") {
		switch {
		case strings.HasPrefix(l, "- name: "):
			cases = append(cases, overlapSpecCase{name: strings.TrimPrefix(l, "- name: ")})
			c = &cases[len(cases)-1]
			inQuery = false
		case strings.HasPrefix(l, "  schema: "):
			c.schema, _ = strconv.Atoi(strings.TrimPrefix(l, "  schema: "))
		case strings.HasPrefix(l, "  query: "):
			inQuery = true
		case strings.HasPrefix(l, "  errors:"):
			inQuery = false
		case inQuery:
			c.query += strings.TrimPrefix(l, "    ") + "\n"
		}
	}
	return schemas, cases
}

// Every case graphql-js specifies for this rule, through both rules: they
// must agree on which documents are valid. Messages differ only in count --
// fieldsCanMergeRule reports one conflict per response path where gqlparser
// reports every conflicting pair -- and only documents above
// overlapExactMaxSelections ever see fieldsCanMergeRule's.
func TestFieldsCanMergeAgreesWithGqlparserSpec(t *testing.T) {
	schemas, cases := readOverlapSpec(t, gqlparserSpecDir(t))
	if len(cases) < 40 {
		t.Fatalf("read %d cases; the spec file's format has changed", len(cases))
	}
	for _, c := range cases {
		want, err := validUnder(schemas[c.schema], c.query, gqlparserOverlap)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, _ := validUnder(schemas[c.schema], c.query, fieldsCanMergeRule)
		if got != want {
			t.Errorf("%s: valid = %v, gqlparser says %v\n%s", c.name, got, want, c.query)
		}
	}
}

const overlapFuzzSDL = `
interface Pet { name(surname: Boolean): String  owner: Human  friends: [Pet] }
type Dog implements Pet {
	name(surname: Boolean): String  owner: Human  friends: [Pet]
	nickname: String  barkVolume: Int  bark: Int  scalar: String  tags: [String]  box: Box
}
type Cat implements Pet {
	name(surname: Boolean): String  owner: Human  friends: [Pet]
	nickname: String  meowVolume: Int  scalar: Int  tags: [Int]  box: [Box]
}
type Human { name: String  pets: [Pet]  best: Pet  scalar: String! }
type Box { scalar: String  deep: Box  other: Int }
union Being = Dog | Cat | Human
type Query { pet: Pet  being: Being  dog: Dog  human: Human }
`

// overlapDoc builds a random document over overlapFuzzSDL in which response
// names collide often: aliases come from three names, fields from the type
// they are selected on, and fragments and inline fragments on every type.
type overlapDoc struct {
	r         *rand.Rand
	s         *ast.Schema
	fragments []string
	nfrag     int
}

func (d *overlapDoc) selections(typ string, depth int) string {
	def := d.s.Types[typ]
	var b strings.Builder
	b.WriteString("{")
	for range 1 + d.r.IntN(3) {
		switch k := d.r.IntN(10); {
		case k < 6 && def.Kind != ast.Union:
			f := def.Fields[d.r.IntN(len(def.Fields))]
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			if d.r.IntN(2) == 0 {
				fmt.Fprintf(&b, " %s:", []string{"x", "y", "name"}[d.r.IntN(3)])
			}
			b.WriteString(" " + f.Name)
			if len(f.Arguments) > 0 && d.r.IntN(2) == 0 {
				fmt.Fprintf(&b, "(surname: %v)", d.r.IntN(2) == 0)
			}
			if inner := d.s.Types[f.Type.Name()]; inner.Kind != ast.Scalar && inner.Kind != ast.Enum {
				if depth > 2 {
					b.WriteString(" { __typename }")
				} else {
					b.WriteString(" " + d.selections(inner.Name, depth+1))
				}
			}
		case k < 9 && depth < 4:
			on := d.possible(def)
			fmt.Fprintf(&b, " ... on %s %s", on, d.selections(on, depth+1))
		case depth < 4:
			on := d.possible(def)
			name := fmt.Sprintf("F%d", d.nfrag)
			d.nfrag++
			d.fragments = append(d.fragments, fmt.Sprintf("fragment %s on %s %s", name, on, d.selections(on, depth+1)))
			b.WriteString(" ..." + name)
		}
	}
	if b.Len() == 1 {
		b.WriteString(" __typename")
	}
	b.WriteString(" }")
	return b.String()
}

// possible is a type a fragment inside def can be on without being refused
// by PossibleFragmentSpreads, which would only add noise.
func (d *overlapDoc) possible(def *ast.Definition) string {
	switch def.Name {
	case "Pet":
		return []string{"Pet", "Dog", "Cat"}[d.r.IntN(3)]
	case "Being":
		return []string{"Dog", "Cat", "Human", "Pet"}[d.r.IntN(4)]
	case "Dog", "Cat":
		return []string{def.Name, "Pet"}[d.r.IntN(2)]
	}
	return def.Name
}

func randomOverlapDoc(s *ast.Schema, seed uint64) string {
	d := &overlapDoc{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), s: s}
	q := "query " + d.selections("Query", 0)
	return q + "\n" + strings.Join(d.fragments, "\n")
}

// Random documents built to collide, through both rules. The spec cases are
// hand-written and few; this is what finds a disagreement neither author
// thought to write down.
func TestFieldsCanMergeAgreesWithGqlparserRandom(t *testing.T) {
	s := gqlparser.MustLoadSchema(&ast.Source{Input: overlapFuzzSDL})
	n := 20000
	if testing.Short() {
		n = 2000
	}
	valid, invalid := 0, 0
	for seed := range uint64(n) {
		q := randomOverlapDoc(s, seed)
		want, err := validUnder(s, q, gqlparserOverlap)
		if err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, q)
		}
		got, _ := validUnder(s, q, fieldsCanMergeRule)
		if got != want {
			t.Fatalf("seed %d: valid = %v, gqlparser says %v\n%s", seed, got, want, q)
		}
		if want {
			valid++
		} else {
			invalid++
		}
	}
	// A generator that only ever produced one verdict would agree trivially.
	if valid < n/10 || invalid < n/10 {
		t.Fatalf("generator is lopsided: %d valid, %d invalid", valid, invalid)
	}
	t.Logf("%d valid, %d invalid, all agreed", valid, invalid)
}

func FuzzFieldsCanMerge(f *testing.F) {
	s := gqlparser.MustLoadSchema(&ast.Source{Input: overlapFuzzSDL})
	for seed := range uint64(8) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		q := randomOverlapDoc(s, seed)
		want, err := validUnder(s, q, gqlparserOverlap)
		if err != nil {
			t.Skip()
		}
		if got, _ := validUnder(s, q, fieldsCanMergeRule); got != want {
			t.Fatalf("valid = %v, gqlparser says %v\n%s", got, want, q)
		}
	})
}

// mergeRuleWithin is fieldsCanMergeRule with its unions held to limit indexed
// sets rather than maxSetProbes. At one, every union is flat; at two or three
// most are, and a fragment's set is often the part made flat for the union
// that spreads it. Those are the paths a document takes only when it is wide,
// which a random one small enough to compare never is.
func mergeRuleWithin(limit int) core.Rule {
	return core.Rule{Name: fieldsCanMergeRule.Name, RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		m := newMergeCheck()
		m.limit = limit
		m.observe(observers, addError)
	}}
}

// overlapWideSDL has what overlapFuzzSDL does not: every wrapping of a list
// and a non-null, enums, an interface implementing another, and arguments
// that are lists, input objects and variables.
const overlapWideSDL = `
enum Color { RED GREEN }
enum Size { S M }
input In { a: Int b: [Int] c: In }
interface Named { name(x: Int, y: [Int], in: In, c: Color): String }
interface Pet implements Named { name(x: Int, y: [Int], in: In, c: Color): String friends: [Pet] kind: Color }
type Dog implements Pet & Named {
	name(x: Int, y: [Int], in: In, c: Color): String friends: [Pet] kind: Color
	a: Int b: Int! c: [Int] d: [Int]! e: [Int!] f: [Int!]! g: [[Int]] h: [[Int]!] s: String col: Color sz: Size
	box: Box boxes: [Box] boxesNN: [Box]! boxNN: Box! boxes2: [[Box]] owner: Human
}
type Cat implements Pet & Named {
	name(x: Int, y: [Int], in: In, c: Color): String friends: [Pet] kind: Color
	a: String b: Int c: [String] d: [Int] e: [Int] f: [Int!] g: [Int] h: [[Int]] s: String! col: Size sz: Size
	box: Box boxes: [Box] boxesNN: [Box] boxNN: Box boxes2: [Box] owner: Human
}
type Human implements Named { name(x: Int, y: [Int], in: In, c: Color): String pets: [Pet] best: Pet a: Int s: String box: Box! }
type Box { a: Int b: String deep: Box boxes: [Box] s: String! col: Color }
union Being = Dog | Cat | Human
type Query { pet: Pet being: Being dog: Dog human: Human named: Named }
`

// wideOverlapDoc builds a random document over overlapWideSDL. Unlike
// overlapDoc it spreads a fragment in more than one place, which is what the
// rule shares its work over, and now and then selects a field its parent does
// not define, which the rule keeps and compares by name.
type wideOverlapDoc struct {
	r         *rand.Rand
	s         *ast.Schema
	fragments []string
	byType    map[string][]string
	calm      bool
}

func (d *wideOverlapDoc) args() string {
	values := map[string][]string{
		"x":  {"1", "2", "$v", "$w", "null"},
		"y":  {"[1]", "[1,2]", "[2,1]", "[$v]", "1", "[]"},
		"in": {"{a:1}", "{a:1,b:[1]}", "{b:[1],a:1}", "{c:{a:1}}", "{a:$v}", "{}"},
		"c":  {"RED", "GREEN"},
	}
	var parts []string
	for _, n := range []string{"x", "y", "in", "c"} {
		if d.r.IntN(4) == 0 {
			parts = append(parts, n+": "+values[n][d.r.IntN(len(values[n]))])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	d.r.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
	return "(" + strings.Join(parts, ", ") + ")"
}

func (d *wideOverlapDoc) selections(typ string, depth int) string {
	def := d.s.Types[typ]
	var b strings.Builder
	b.WriteString("{")
	for range 1 + d.r.IntN(4) {
		switch k := d.r.IntN(12); {
		case k == 0 && def.Kind != ast.Union:
			b.WriteString(" x: undefinedField")
		case k < 7 && def.Kind != ast.Union:
			f := def.Fields[d.r.IntN(len(def.Fields))]
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			// A calm document aliases rarely and passes no arguments, so
			// that enough of them are valid.
			if d.r.IntN(2) == 0 && (!d.calm || d.r.IntN(4) == 0) {
				fmt.Fprintf(&b, " %s:", []string{"x", "y", "name", "a"}[d.r.IntN(4)])
			}
			b.WriteString(" " + f.Name)
			if len(f.Arguments) > 0 && !d.calm {
				b.WriteString(d.args())
			}
			if inner := d.s.Types[f.Type.Name()]; inner.Kind != ast.Scalar && inner.Kind != ast.Enum {
				if depth > 2 {
					b.WriteString(" { __typename }")
				} else {
					b.WriteString(" " + d.selections(inner.Name, depth+1))
				}
			}
		case k < 9 && depth < 4:
			on := d.possible(def)
			fmt.Fprintf(&b, " ... on %s %s", on, d.selections(on, depth+1))
		case depth < 4:
			on := d.possible(def)
			// Only a finished fragment is spread again, so none reaches
			// itself.
			if known := d.byType[on]; len(known) > 0 && d.r.IntN(2) == 0 {
				b.WriteString(" ..." + known[d.r.IntN(len(known))])
				continue
			}
			body := d.selections(on, depth+1)
			name := fmt.Sprintf("F%d", len(d.fragments))
			d.fragments = append(d.fragments, fmt.Sprintf("fragment %s on %s %s", name, on, body))
			d.byType[on] = append(d.byType[on], name)
			b.WriteString(" ..." + name)
		}
	}
	if b.Len() == 1 {
		b.WriteString(" __typename")
	}
	b.WriteString(" }")
	return b.String()
}

func (d *wideOverlapDoc) possible(def *ast.Definition) string {
	switch def.Name {
	case "Named":
		return []string{"Named", "Pet", "Dog", "Cat", "Human"}[d.r.IntN(5)]
	case "Pet":
		return []string{"Pet", "Dog", "Cat", "Named"}[d.r.IntN(4)]
	case "Being":
		return []string{"Dog", "Cat", "Human", "Pet", "Named"}[d.r.IntN(5)]
	case "Dog", "Cat":
		return []string{def.Name, "Pet", "Named"}[d.r.IntN(3)]
	case "Human":
		return []string{"Human", "Named"}[d.r.IntN(2)]
	}
	return def.Name
}

func randomWideOverlapDoc(s *ast.Schema, seed uint64) string {
	d := &wideOverlapDoc{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), s: s, byType: map[string][]string{}, calm: seed%2 == 0}
	q := "query($v: Int, $w: Int) " + d.selections("Query", 0)
	return q + "\n" + strings.Join(d.fragments, "\n")
}

// mergeVerdicts runs q through gqlparser's rule, the flat rule this one
// replaced, and this one at its own probe limit and at limits low enough to
// make every union take the wide paths. They answer one question.
func mergeVerdicts(s *ast.Schema, q string) (want bool, got [4]bool, err error) {
	if want, err = validUnder(s, q, gqlparserOverlap); err != nil {
		return false, got, err
	}
	for i, r := range []core.Rule{flatFieldsCanMergeRule, fieldsCanMergeRule, mergeRuleWithin(1), mergeRuleWithin(3)} {
		got[i], _ = validUnder(s, q, r)
	}
	return want, got, nil
}

// The rule that shares its work over fragments, against the one that wrote
// every selection set out and against gqlparser's, on documents that spread
// their fragments in several places.
func TestFieldsCanMergeAgreesWithTheFlatRule(t *testing.T) {
	s := gqlparser.MustLoadSchema(&ast.Source{Input: overlapWideSDL})
	n := 12000
	if testing.Short() {
		n = 1500
	}
	valid, invalid := 0, 0
	for seed := range uint64(n) {
		q := randomWideOverlapDoc(s, seed)
		want, got, err := mergeVerdicts(s, q)
		if err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, q)
		}
		if got != [4]bool{want, want, want, want} {
			t.Fatalf("seed %d: gqlparser says valid = %v; the flat rule, this rule, and this rule at limits 1 and 3 say %v\n%s", seed, want, got, q)
		}
		if want {
			valid++
		} else {
			invalid++
		}
	}
	if valid < n/10 || invalid < n/10 {
		t.Fatalf("generator is lopsided: %d valid, %d invalid", valid, invalid)
	}
	t.Logf("%d valid, %d invalid, all agreed", valid, invalid)
}

func FuzzFieldsCanMergeWide(f *testing.F) {
	s := gqlparser.MustLoadSchema(&ast.Source{Input: overlapWideSDL})
	for seed := range uint64(8) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		q := randomWideOverlapDoc(s, seed)
		want, got, err := mergeVerdicts(s, q)
		if err != nil {
			t.Skip()
		}
		if got != [4]bool{want, want, want, want} {
			t.Fatalf("gqlparser says valid = %v; the flat rule, this rule, and this rule at limits 1 and 3 say %v\n%s", want, got, q)
		}
	})
}

// mergeSteps is the work the rule does on q, by its own count.
func mergeSteps(t *testing.T, s *ast.Schema, q string) int {
	t.Helper()
	doc, err := parser.ParseQuery(&ast.Source{Input: q})
	if err != nil {
		t.Fatalf("%v\n%.200s", err, q)
	}
	m := newMergeCheck()
	rule := core.Rule{Name: fieldsCanMergeRule.Name, RuleFunc: m.observe}
	if errs := validator.ValidateWithRules(s, doc, rules.NewRules(rule)); len(errs) > 0 {
		t.Fatalf("%v\n%.200s", errs[0], q)
	}
	return m.steps
}

// mergeShapes are documents that share fragments between selection sets, in
// the ways that made a rule which writes each selection set out quadratic.
// Each is a function of n and about n times some constant long.
var mergeShapes = map[string]func(n int) string{
	"one fragment under many fields": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("u%d:me{...F} ", i) }) + "} fragment F on User{" +
			repeated(7*n, func(int) string { return "id " }) + "}"
	},
	"beside a field of its own": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("u%d:me{s%d:id ...F} ", i, i) }) + "} fragment F on User{" +
			repeated(3*n, func(i int) string { return fmt.Sprintf("a%d:id ", i) }) + "}"
	},
	"two fragments beside a field": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("u%d:me{s%d:id ...F ...G} ", i, i) }) + "} fragment F on User{" +
			repeated(6*n, func(int) string { return "id " }) + "} fragment G on User{" + repeated(6*n, func(int) string { return "name " }) + "}"
	},
	// No two fields spread the same fragments, so nothing is shared at the
	// level of a selection set; what is shared is each pair of fragments.
	"nine of twelve fragments": func(n int) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			fmt.Fprintf(&b, "u%d:me{", i)
			for j := range 9 {
				fmt.Fprintf(&b, "...F%d ", (i+j*(1+i%3))%12)
			}
			b.WriteString("} ")
		}
		b.WriteString("}")
		for j := range 12 {
			fmt.Fprintf(&b, " fragment F%d on User{%s}", j, repeated(n, func(int) string { return "id " }))
		}
		return b.String()
	},
	"a fragment of fragments under many fields": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("u%d:me{s%d:id ...R} ", i, i) }) + "} fragment R on User{" +
			repeated(n, func(i int) string { return fmt.Sprintf("...P%d ", i) }) + "}" +
			repeated(n, func(i int) string { return fmt.Sprintf(" fragment P%d on User{id}", i) })
	},
	// R is a union of exactly maxSetProbes sets, so with the field beside it
	// each selection set is one over the limit: what has to give is R, which
	// they all share, not the selection set.
	"a fragment of sixty-four under many fields": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("u%d:me{s%d:id ...R} ", i, i) }) + "} fragment R on User{" +
			repeated(maxSetProbes, func(i int) string { return fmt.Sprintf("...P%d ", i) }) + "}" +
			repeated(maxSetProbes, func(i int) string {
				return fmt.Sprintf(" fragment P%d on User{%s}", i, repeated(1+n/8, func(int) string { return "id " }))
			})
	},
	"small fragments that each spread a large one": func(n int) string {
		return "{me{" + repeated(n, func(i int) string { return fmt.Sprintf("...P%d ", i) }) + "}}" +
			repeated(n, func(i int) string { return fmt.Sprintf(" fragment P%d on User{name ...G}", i) }) +
			" fragment G on User{" + repeated(4*n, func(int) string { return "id " }) + "}"
	},
	"one response name over many selection sets": func(n int) string {
		return "{" + repeated(n, func(i int) string { return fmt.Sprintf("me{a%d:id ...F} ", i) }) + "} fragment F on User{" +
			repeated(7*n, func(int) string { return "id " }) + "}"
	},
	"each fragment spreads the next": func(n int) string {
		return "{me{...F0}}" + repeated(n, func(i int) string { return fmt.Sprintf(" fragment F%d on User{id name nick ...F%d}", i, i+1) }) +
			fmt.Sprintf(" fragment F%d on User{id}", n)
	},
}

// The rule's work is counted rather than timed, and held to growing as the
// document does: twice the document, about twice the steps. The rule it
// replaced did four times the work for these, since each of n selection sets
// was written out with the n fields of the fragment it spread.
func TestMergeRuleWorkGrowsWithTheDocument(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for name, gen := range mergeShapes {
		t.Run(name, func(t *testing.T) {
			const n = 200
			once, twice := mergeSteps(t, e.schema.ast, gen(n)), mergeSteps(t, e.schema.ast, gen(2*n))
			t.Logf("%d steps for n=%d, %d for n=%d: x%.2f", once, n, twice, 2*n, float64(twice)/float64(once))
			if float64(twice) > 2.3*float64(once) {
				t.Fatalf("the document doubled and the work went from %d steps to %d", once, twice)
			}
		})
	}
}

// What a ratio cannot show is the scale, so the largest document of each
// shape the default limits accept is also held to a bound on the count
// itself. The widest thing the rule does is ask every indexed set of a union
// about one response name, so maxSetProbes steps a token is generous; the
// flat rule took over five hundred a token on the first of these.
func TestMergeRuleWorkIsBoundedByTheDocument(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for name, gen := range mergeShapes {
		q := largestAccepted(gen)
		steps, tokens := mergeSteps(t, e.schema.ast, q), countTokens(q)
		t.Logf("%-46s %d tokens, %d steps", name, tokens, steps)
		if steps > maxSetProbes*tokens {
			t.Errorf("%s: %d steps for %d tokens", name, steps, tokens)
		}
	}
}

// Shapes the random documents are too small or too regular to contain, each
// through gqlparser's rule and this one at every probe limit: fragments that
// reach themselves, a leaf pair with a composite field between them, a field
// its parent does not define, and the same again with the fields apart in
// fragments, where they meet as sets rather than inside one.
func TestFieldsCanMergeAgreesWithGqlparserOnShapesByHand(t *testing.T) {
	s := gqlparser.MustLoadSchema(&ast.Source{Input: overlapWideSDL})
	for _, tc := range []struct {
		name  string
		query string
		valid bool
	}{
		{"a leaf pair behind a composite", `{ being { ... on Dog { x: box { a } } ... on Cat { x: a } ... on Human { x: a } } }`, false},
		{"the same, in fragments", `{ being { ...D ...C ...H } } fragment D on Dog { x: box { a } } fragment C on Cat { x: a } fragment H on Human { x: a }`, false},
		{"a composite and one leaf type", `{ being { ...D ...C ...C2 } } fragment D on Dog { x: box { a } } fragment C on Cat { x: a } fragment C2 on Cat { x: a }`, true},
		{"wrapping apart in fragments", `{ being { ...D ...C } } fragment D on Dog { x: g } fragment C on Cat { x: g }`, false},
		{"a list's own nullability apart", `{ being { ...D ...C } } fragment D on Dog { x: d } fragment C on Cat { x: d }`, true},
		{"arguments apart in fragments", `{ dog { ...A ...B } } fragment A on Dog { name(x: 1) } fragment B on Dog { name(x: 2) }`, false},
		{"arguments on different object types", `{ pet { ...A ...B } } fragment A on Dog { name(x: 1) } fragment B on Cat { name(x: 2) }`, true},
		{"an abstract field between two object types", `{ pet { ...A ...B ...P } } fragment A on Dog { name(x: 1) } fragment B on Cat { name(x: 2) } fragment P on Pet { name(x: 1) }`, false},
		{"an abstract field agreeing with both", `{ pet { ...A ...B ...P } } fragment A on Dog { name(x: 1) } fragment B on Cat { name(x: 1) } fragment P on Pet { name(x: 1) }`, true},
		{"subselections of fields on different types", `{ pet { ...A ...B } } fragment A on Dog { box { x: a } } fragment B on Cat { box { x: b } }`, false},
		{"subselections agree in shape, differ in name", `{ pet { ...A ...B } } fragment A on Dog { owner { x: name(x: 1) } } fragment B on Cat { owner { x: name(x: 2) } }`, true},
		{"the same under one type", `{ dog { ...A ...B } } fragment A on Dog { owner { x: name(x: 1) } } fragment B on Dog { owner { x: name(x: 2) } }`, false},
		// A field on an object type and one on its interface could share a
		// parent, so their subselections are held to the same; either set
		// may be the one the other is looked up in.
		{"subselections under an object type's field and an abstract one", `{ pet { ...A ...P } } fragment A on Dog { friends { x: name(x: 1) } } fragment P on Pet { friends { x: name(x: 2) } }`, false},
		{"the same, spread in the other order", `{ pet { ...P ...A } } fragment A on Dog { friends { x: name(x: 1) } } fragment P on Pet { friends { x: name(x: 2) } }`, false},
		{"the same, agreeing", `{ pet { ...P ...A } } fragment A on Dog { friends { x: name(x: 1) } } fragment P on Pet { friends { x: name(x: 1) } }`, true},
		{"an undefined field beside a defined one", `{ dog { ...A ...B } } fragment A on Dog { x: undefinedField } fragment B on Dog { x: a }`, false},
		// Three fragments that are mostly the one they all spread: at a low
		// limit they are indexed together rather than each on its own.
		{"fragments sharing most of their fields", `{ dog { ...A ...B ...C } } fragment A on Dog { ...G x1: a } fragment B on Dog { ...G x2: a }
			fragment C on Dog { ...G x: a } fragment G on Dog { x: a b c d e f g h }`, true},
		{"a conflict between one of them and what they share", `{ dog { ...A ...B ...C } } fragment A on Dog { ...G x1: a } fragment B on Dog { ...G x2: a }
			fragment C on Dog { ...G x: s } fragment G on Dog { x: a b c d e f g h }`, false},
		{"a conflict between two of them", `{ dog { ...A ...B ...C } } fragment A on Dog { ...G y: a } fragment B on Dog { ...G x2: a }
			fragment C on Dog { ...G y: s } fragment G on Dog { x: a b c d e f g h }`, false},
		{"a fragment that spreads itself", `{ dog { ...A } } fragment A on Dog { a ...A }`, true},
		{"a conflict inside a fragment that spreads itself", `{ dog { ...A } } fragment A on Dog { x: a x: b ...A }`, false},
		{"two fragments that spread each other", `{ dog { ...A } } fragment A on Dog { x: a ...B } fragment B on Dog { x: s ...A }`, false},
		{"a fragment that reaches itself through a field", `{ dog { ...A } } fragment A on Dog { box { deep { a } } owner { best { ...P } } } fragment P on Pet { ... on Dog { ...A } }`, true},
		{"a cycle that agrees", `{ dog { ...A } } fragment A on Dog { x: a ...B } fragment B on Dog { x: a ...C } fragment C on Dog { y: b ...A }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := validUnder(s, tc.query, gqlparserOverlap)
			if err != nil {
				t.Fatal(err)
			}
			if want != tc.valid {
				t.Fatalf("gqlparser says valid = %v, the case expects %v", want, tc.valid)
			}
			rs := map[string]core.Rule{"the flat rule": flatFieldsCanMergeRule, "limit 64": fieldsCanMergeRule}
			for _, limit := range []int{1, 2, 3} {
				rs[fmt.Sprintf("limit %d", limit)] = mergeRuleWithin(limit)
			}
			for name, r := range rs {
				if got, _ := validUnder(s, tc.query, r); got != want {
					t.Errorf("%s: valid = %v, gqlparser says %v", name, got, want)
				}
			}
		})
	}
}

// Below overlapExactMaxSelections a document is checked by gqlparser's own
// rule, which reports every conflicting pair; above it by this one, which
// reports each group once. The count is the visible difference, and it is
// what says which rule a document of each size was given.
func TestSmallDocumentsKeepGqlparsersMergeMessages(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const conflict = "a: echo(v: 1) a: echo(v: 2) a: echo(v: 3) "
	small := "{ " + conflict + "}"
	large := "{ " + conflict + repeated(overlapExactMaxSelections, func(i int) string { return fmt.Sprintf("s%d: strings ", i) }) + "}"

	doc := mustParse(small)
	want := validator.ValidateWithRules(e.schema.ast, doc, rules.NewRules(gqlparserOverlap))
	_, got := e.document(small)
	if len(want) != 3 || len(got) != len(want) {
		t.Fatalf("a small document: %d errors, gqlparser's rule reports %d and three were expected: %s", len(got), len(want), errorsJSON(got))
	}
	for i := range got {
		if got[i].Message != want[i].Message {
			t.Fatalf("error %d = %q, gqlparser's rule says %q", i, got[i].Message, want[i].Message)
		}
	}
	if _, got := e.document(large); len(got) != 1 {
		t.Fatalf("a large document: %d errors, want the one this rule reports for the group: %s", len(got), errorsJSON(got))
	}
}
