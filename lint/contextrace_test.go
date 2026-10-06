package lint_test

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/syssam/graphql-go/lint"
)

func TestContextRace(t *testing.T) {
	// Point the analyzer at the fixture's own type so the test does not have
	// to depend on graphql-go, and put it back: the tests below are about
	// the name it ships with.
	shipped := lint.ContextType
	t.Cleanup(func() { lint.ContextType = shipped })
	lint.ContextType = "a.OperationContext"
	analysistest.Run(t, analysistest.TestData(), lint.ContextRace, "a")
}

// The variants, against a stand-in at the real import path so that this
// module does not have to require graphql-go.
func TestContextRaceVariants(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), lint.ContextRace, "b")
}

// A stand-in cannot notice the real type being renamed, or the name here
// going stale: gqlvet would match nothing and report a clean tree for ever.
// This runs the analyzer as shipped over the control package in the root
// module, which imports the real graphql.OperationContext, and requires a
// finding on every line marked for one.
func TestContextRaceOnTheRealContext(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not in the graphql-go repository: there is no root module beside this one")
	}
	const control = "scripts/testdata/gqlvet"

	pkgs, err := packages.Load(&packages.Config{
		Dir: root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports |
			packages.NeedTypes | packages.NeedTypesSizes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
	}, "./"+control)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("loaded %d packages from %s, want 1", len(pkgs), control)
	}
	for _, e := range pkgs[0].Errors {
		t.Errorf("%s does not build: %v", control, e)
	}
	if t.Failed() {
		t.FailNow()
	}

	graph, err := checker.Analyze([]*analysis.Analyzer{lint.ContextRace}, pkgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, act := range graph.Roots {
		if act.Err != nil {
			t.Fatal(act.Err)
		}
		for _, d := range act.Diagnostics {
			got = append(got, pkgs[0].Fset.Position(d.Pos).Line)
		}
	}
	slices.Sort(got)

	want := wantLines(t, filepath.Join(root, control, "control.go"))
	if len(want) == 0 {
		t.Fatalf("%s marks no line: the control is not one", control)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings on lines %v of %s, want %v.\nContextType is %q; if no line was found, that no longer names the type the root package exports.",
			got, control, want, lint.ContextType)
	}
}

func wantLines(t *testing.T, file string) []int {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []int
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "// want") {
			lines = append(lines, n)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}
