package codegen

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// In the group's own package the author's code shares a namespace with
// generated.go. A hand-written name gqlc also emits is named, file and all,
// instead of surfacing as a redeclaration inside a file marked DO NOT EDIT.
func TestScaffoldNamesAHandWrittenClashWithGeneratedCode(t *testing.T) {
	dir, generate := scaffoldModule(t, map[string]string{"post": "graph/post.Handler"})
	writeUnder(t, dir, "graph/post/extra.go", "package post\n\ntype PostArgs struct{}\n")
	err := generate()
	if err == nil || !strings.Contains(err.Error(), "extra.go") || !strings.Contains(err.Error(), "PostArgs") {
		t.Errorf("err = %v", err)
	}
}

// One implementation spelled two ways is one implementation: judged apart,
// each spelling called the other group's live methods stale.
func TestScaffoldTargetSpellingsAreOneImplementation(t *testing.T) {
	dir, _ := scaffoldModule(t, nil)
	notes := scaffoldNotes(t, dir, map[string]string{"user": "impl.Resolver", "post": "./impl/.Resolver"})
	for _, n := range notes {
		if strings.Contains(n, "has methods") {
			t.Errorf("a live method was called stale: %s", n)
		}
	}
}

// A method no Resolver of a shared type asks for is reported once for the
// type, naming every group it serves -- not once per group, each naming one
// interface as if it alone had been checked.
func TestScaffoldReportsAStaleMethodOncePerType(t *testing.T) {
	dir, _ := scaffoldModule(t, nil)
	targets := map[string]string{"user": "impl.Resolver", "post": "impl.Resolver"}
	scaffoldNotes(t, dir, targets)
	writeUnder(t, dir, "impl/extra.go", "package impl\n\nfunc (r *Resolver) Archived() bool { return false }\n")
	var stale []string
	for _, n := range scaffoldNotes(t, dir, targets) {
		if strings.Contains(n, "has methods") {
			stale = append(stale, n)
		}
	}
	if len(stale) != 1 || !strings.Contains(stale[0], "none of the post, user Resolvers has: Archived") {
		t.Errorf("stale notes = %q", stale)
	}
}

// Where case matters, impl and Impl are two packages, each with its own R, and
// a stale method on either must be reported: merged as one implementation,
// Impl's was never looked at. Skipped where case is ignored, where they are
// one directory.
func TestScaffoldKeepsTargetsApartWhereCaseMatters(t *testing.T) {
	dir, _ := scaffoldModule(t, nil)
	if caseInsensitive(t, dir) {
		t.Skip("case-insensitive filesystem: impl and Impl are one directory")
	}
	// post sorts first, so a merged pair is judged by Impl's methods alone; the
	// stale method goes in impl, the one that would then never be looked at.
	targets := map[string]string{"user": "impl.R", "post": "Impl.R"}
	scaffoldNotes(t, dir, targets)
	writeUnder(t, dir, "impl/extra.go", "package impl\n\nfunc (r *R) Archived() bool { return false }\n")
	var found bool
	for _, n := range scaffoldNotes(t, dir, targets) {
		found = found || (strings.Contains(n, "impl.R") && strings.Contains(n, "Archived"))
	}
	if !found {
		t.Error("the stale method on impl.R was not reported")
	}
}

// Where case is ignored, Impl and impl are one package, and every group
// scaffolding into it must see what the others wrote: with an index per
// spelling, gamma read Impl's stale copy, missed the T beta had just written
// to impl, and declared T again. Skipped where case matters.
func TestScaffoldOnePackageUnderEverySpelling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir := t.TempDir()
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive filesystem: Impl and impl are two packages")
	}
	writeUnder(t, dir, "schema/query.graphql", "type Query { ping: Boolean }\n")
	for _, g := range []string{"alpha", "beta", "gamma"} {
		writeUnder(t, dir, "schema/"+g+".graphql", "type "+strings.ToUpper(g[:1])+g[1:]+" { id: ID! }\nextend type Query { "+g+": "+strings.ToUpper(g[:1])+g[1:]+" }\n")
	}
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"alpha": "Impl.U", "beta": "impl.T", "gamma": "Impl.T", "query": "impl.Q"},
	}); err != nil {
		t.Fatal(err)
	}
	goBuild(t, dir)
}

// A file the build leaves out is not part of the package scaffold writes
// into: its package clause does not name a new stub file, and its
// declarations do not clash with generated code.
func TestScaffoldIgnoresFilesTheBuildLeavesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := scaffoldModule(t, map[string]string{"user": "impl.UserResolver", "post": "graph/post.Handler"})
	// Sorts after user.resolvers.go, so its clause is the last one read.
	writeUnder(t, dir, "impl/zz_tool.go", "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	writeUnder(t, dir, "graph/post/zz_gen.go", "//go:build ignore\n\npackage main\n\ntype PostArgs struct{}\n")
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "impl", "user.resolvers.go")); !strings.HasPrefix(got, "package impl\n") {
		t.Errorf("the new stub file took an excluded file's package clause:\n%.60s", got)
	}
	goBuild(t, dir)
}

// A file built only on another platform is part of the package on that one,
// so what it declares is declared: judged by the machine running gqlc, a type
// declared only there was declared again, and the build where it exists
// failed. plan9 is a platform no test machine is.
func TestScaffoldCountsFilesBuiltElsewhere(t *testing.T) {
	dir, generate := scaffoldModule(t, map[string]string{"user": "impl.UserResolver"})
	writeUnder(t, dir, "impl/zz_plan9.go", "//go:build plan9\n\npackage impl\n\ntype UserResolver struct{}\n\nfunc (r *UserResolver) Users() {}\n")
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "impl", "user.resolvers.go"))
	for _, dup := range []string{"type UserResolver struct", ") Users(ctx"} {
		if strings.Contains(got, dup) {
			t.Errorf("declared again what a plan9-only file declares: %q\n%s", dup, got)
		}
	}
}

func scaffoldNotes(t *testing.T, dir string, scaffold map[string]string) []string {
	t.Helper()
	var notes []string
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold,
		Notef:    func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
	}); err != nil {
		t.Fatal(err)
	}
	return notes
}
