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
