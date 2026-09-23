package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

// manyGroupSchema writes n SDL files, so the generator produces n groups. The
// default grouping is by file stem, which is how a large schema arrives: one
// file per module, hundreds of them.
func manyGroupSchema(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	var q strings.Builder
	q.WriteString("type Query {\n")
	for i := range n {
		body := fmt.Sprintf("type M%02dT { id: ID! name: String! }\ninput M%02dIn { q: String }\nenum M%02dE { A B }\n", i, i, i)
		if err := os.WriteFile(filepath.Join(dir, "schema", fmt.Sprintf("mod%02d.graphql", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&q, "  q%02d: M%02dT\n", i, i)
	}
	q.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(dir, "schema", "query.graphql"), []byte(q.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// typeNames scans every type in the schema and sorts the result, and emit asks
// for it at a dozen call sites -- once per group. At 800 groups over 4 800
// types that made generation quadratic: 12.7s where the memoized version takes
// 3.2s, and the gap widens with every type added.
//
// A timing test for that would be flaky, so this counts the scans instead.
// There are six kinds of type, so six scans is the whole budget however many
// groups there are.
func TestTypeNamesIsComputedOncePerKind(t *testing.T) {
	const groups = 30
	dir := manyGroupSchema(t, groups)

	b, err := newBuilder(dir, Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema/*.graphql"},
		Output:      "graph",
		Package:     "example.com/s/graph",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Ask the way emit does: every kind, once per group.
	kinds := []ast.DefinitionKind{ast.Object, ast.InputObject, ast.Enum, ast.Scalar, ast.Interface, ast.Union}
	for range groups {
		for _, k := range kinds {
			b.typeNames(k)
		}
	}

	if b.nameScans > len(kinds) {
		t.Errorf("typeNames scanned %d times for %d kinds over %d groups; it must scan once per kind",
			b.nameScans, len(kinds), groups)
	}
}

// groupOf is asked once per type per group, which at the scale above is
// millions of calls. It reads the type's source file and sanitizes the stem,
// neither of which changes once the schema is loaded.
func TestGroupOfIsMemoized(t *testing.T) {
	dir := manyGroupSchema(t, 5)
	b, err := newBuilder(dir, Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema/*.graphql"},
		Output:      "graph",
		Package:     "example.com/s/graph",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := b.groupOf("M00T")
	for range 100 {
		if got := b.groupOf("M00T"); got != first {
			t.Fatalf("groupOf returned %q then %q", first, got)
		}
	}
	if len(b.groupByType) != 1 {
		t.Errorf("groupByType holds %d entries after asking about one type", len(b.groupByType))
	}
}

// modelQualifier asks for modelExprImports once per type reference, and that
// scans every Models entry. With AutoBind, Models holds one entry per
// discovered type, so recomputing it is references times types: the real
// 5 503-type schema took 4m58s to generate and 16s once this was memoized.
func TestModelExprImportsIsComputedOnce(t *testing.T) {
	const groups = 30
	dir := manyGroupSchema(t, groups)

	b, err := newBuilder(dir, Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema/*.graphql"},
		Output:      "graph",
		Package:     "example.com/s/graph",
		Models:      map[string]string{"Time": "time.Time"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range b.typeNames(ast.Object) {
		b.modelName(name, "graph")
	}
	if b.exprScans > 1 {
		t.Errorf("modelExprImports was computed %d times; it must be computed once per model map", b.exprScans)
	}

	// And recomputed when the map it reads is replaced, or a type discovered
	// after the first call would be qualified against a stale map.
	b.setModels(map[string]string{"Time": "time.Time", "Extra": "example.com/x/other.Extra"})
	if _, ok := b.modelExprImports()["other"]; !ok {
		t.Error("setModels did not drop the memoized import map")
	}
}

// "gqlc never loads Go packages" is the claim the whole design rests on, and
// only the `len(cfg.AutoBind) > 0` guard keeps it. A regression there costs
// what gqlgen costs -- 31 s and 4.6 GB on a 200-entity schema against under a
// second and 44 MB -- and nothing else in the suite would notice, because
// loading packages makes no output wrong.
func TestNoPackagesAreLoadedWithoutAutoBind(t *testing.T) {
	const groups = 3
	dir := manyGroupSchema(t, groups)
	// A real module with a loadable package in it, so that a load would
	// succeed: the counter is then what fails, and it says what broke, where
	// a load error from an empty directory would send a reader elsewhere.
	writeTempModule(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("package hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := newBuilder(dir, Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema/*.graphql"},
		Output:      "graph",
		Package:     "example.com/s/graph",
		// Named but unloadable: a Models entry is not permission to load.
		Models: map[string]string{"Time": "does.not/exist.Time"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.loads != 0 {
		t.Errorf("packages.Load was called %d times with no AutoBind", b.loads)
	}
}
