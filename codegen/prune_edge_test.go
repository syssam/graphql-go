package codegen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeUnder(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// caseInsensitive reports whether dir's filesystem ignores letter case, as
// Windows' and macOS's do by default.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	_ = os.Remove(probe)
	return err == nil
}

// On a filesystem that ignores case, a file this run writes into a directory
// whose existing spelling differs in case is found by the walk under that old
// spelling. Compared exactly, it was taken for stale and deleted: the run
// removed what it had just generated. Both prunes, the Go files and the
// embedded SDL copies.
func TestPruneKeepsWhatThisRunWroteWhateverTheCase(t *testing.T) {
	dir := t.TempDir()
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive filesystem: the two spellings are two directories")
	}
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "sdl/User.graphql", "type User { id: ID! }\nextend type Query { user: User }\n")
	gen := func(groupDir string) {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph", GroupDir: groupDir,
		}); err != nil {
			t.Fatal(err)
		}
	}
	gen("Register")
	gen("register")
	if _, err := os.Stat(filepath.Join(dir, "graph", "register", "query", "generated.go")); err != nil {
		t.Errorf("the group package written this run was deleted: %v", err)
	}
	// The SDL source renamed only in case: its embedded copy must stay.
	if err := os.Rename(filepath.Join(dir, "sdl", "User.graphql"), filepath.Join(dir, "sdl", "user.graphql")); err != nil {
		t.Fatal(err)
	}
	gen("register")
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "user.graphql")); err != nil {
		t.Errorf("the SDL copy written this run was deleted: %v", err)
	}
}

// The other half of the same rule, where case matters: schema/User.graphql and
// schema/user.graphql are two files, and the one no source produces any more
// must go, or both are embedded and the schema defines User twice. Folding
// case in the compare kept it; only asking the filesystem gets both halves.
// Skipped on a filesystem that ignores case, where the two are one file.
func TestPruneDeletesACaseVariantWhereCaseMatters(t *testing.T) {
	dir := t.TempDir()
	if caseInsensitive(t, dir) {
		t.Skip("case-insensitive filesystem: the two spellings are one file")
	}
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "sdl/User.graphql", "type User { id: ID! }\nextend type Query { user: User }\n")
	gen := func() {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
		}); err != nil {
			t.Fatal(err)
		}
	}
	gen()
	if err := os.Rename(filepath.Join(dir, "sdl", "User.graphql"), filepath.Join(dir, "sdl", "user.graphql")); err != nil {
		t.Fatal(err)
	}
	gen()
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "User.graphql")); !os.IsNotExist(err) {
		t.Errorf("the stale case variant is still embedded (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "user.graphql")); err != nil {
		t.Errorf("the copy written this run is missing: %v", err)
	}
}

// Another module inside Output -- what an Output of "." walks into -- is that
// module's: a gqlc-headed file there is not this run's to delete.
func TestPruneLeavesANestedModuleAlone(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "graph/tool/go.mod", "module example.com/tool\n")
	theirs := filepath.Join(dir, "graph", "tool", "gen", "generated.go")
	writeUnder(t, dir, "graph/tool/gen/generated.go", generatedHeader+"\npackage gen\n")
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a file of a nested module was pruned: %v", err)
	}
}

// A GroupDir reaching into another gqlc run's output root would have the two
// runs prune each other's packages on every regenerate.
func TestGroupDirMayNotReachIntoAnotherRun(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "sdl/user.graphql", "type User { id: ID! }\nextend type Query { user: User }\n")
	writeUnder(t, dir, "graph/admin/schema.go", generatedHeader+"\npackage admin\n")
	writeUnder(t, dir, "graph/admin/schema/admin.graphql", "type Query { admin: Boolean }\n")
	for _, groupDir := range []string{"admin", "admin/gen"} {
		err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph", GroupDir: groupDir,
		})
		if err == nil || !strings.Contains(err.Error(), "another gqlc run") {
			t.Errorf("GroupDir %q: err = %v", groupDir, err)
		}
	}
}

// The walk's own contract, where no filesystem trick is needed: an entry that
// is gone by the time it is visited -- an error the walk reports, or a file
// listed and then removed before its header is read -- is skipped and the
// walk goes on; any other error still stops it.
func TestPruneWalkerSkipsWhatVanished(t *testing.T) {
	dir := t.TempDir()
	var candidates []string
	walk := pruneWalker(dir, writtenKeys(dir, nil), &candidates)
	if err := walk(filepath.Join(dir, "gone"), nil, os.ErrNotExist); err != nil {
		t.Errorf("a vanished entry ended the walk: %v", err)
	}
	// Unreadable is stepped around too: it holds nothing prune could delete,
	// and failing the generate there left every later run failing.
	if err := walk(filepath.Join(dir, "denied"), nil, os.ErrPermission); err != nil {
		t.Errorf("an unreadable entry ended the walk: %v", err)
	}
	if err := walk(filepath.Join(dir, "broken"), nil, errors.New("i/o error")); err == nil {
		t.Error("an error that is neither was swallowed")
	}
	// Listed, then removed before its header is read; beside one still there.
	writeUnder(t, dir, "x/generated.go", generatedHeader+"\npackage x\n")
	writeUnder(t, dir, "y/generated.go", generatedHeader+"\npackage y\n")
	gone, kept := filepath.Join(dir, "x", "generated.go"), filepath.Join(dir, "y", "generated.go")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	ours, err := generatedAmong([]string{gone, kept})
	if err != nil || len(ours) != 1 || ours[0] != kept {
		t.Errorf("generatedAmong = %v, %v: want only %s", ours, err, kept)
	}
}

// Something that vanishes while the walk is under way -- here a link to
// nothing, which the walk finds and cannot open -- is skipped. It used to end
// the walk, reported as success, and every stale file after it survived.
func TestPruneWalksPastWhatVanished(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	gen := func() {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
		}); err != nil {
			t.Fatal(err)
		}
	}
	gen()
	// "a.go" sorts before "zz", so the walk meets the dangling link first.
	if err := os.Symlink(filepath.Join(dir, "nowhere.go"), filepath.Join(dir, "graph", "a.go")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	stale := filepath.Join(dir, "graph", "zz", "generated.go")
	writeUnder(t, dir, "graph/zz/generated.go", generatedHeader+"\npackage zz\n")
	gen()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stale file after a vanished entry survived (stat err %v)", err)
	}
}

// A linked Output -- a symlink, a Windows junction -- is pruned like any
// other: WalkDir does not follow a link at its root, and a glob over schema/
// does, as the embed reading it does. Skipped where links cannot be made.
func TestPruneFollowsALinkedOutput(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "sdl/legacy.graphql", "type Legacy { id: ID! }\nextend type Query { legacy: Legacy }\n")
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "graph")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	gen := func() {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
		}); err != nil {
			t.Fatal(err)
		}
	}
	gen()
	if err := os.Remove(filepath.Join(dir, "sdl", "legacy.graphql")); err != nil {
		t.Fatal(err)
	}
	gen()
	for _, rel := range []string{"schema/legacy.graphql", "legacy/generated.go"} {
		if _, err := os.Stat(filepath.Join(dir, "real", filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s outlived its SDL behind a linked Output (stat err %v)", rel, err)
		}
	}
}

// On a filesystem that ignores case, an existing Schema/ is where this run's
// schema/ copies land, and a stale copy in it is still found and deleted.
func TestPruneFindsSchemaCopiesWhateverTheCase(t *testing.T) {
	dir := t.TempDir()
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive filesystem: Schema and schema are two directories")
	}
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "graph/Schema/old.graphql", "type Old { id: ID! }\n")
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "old.graphql")); !os.IsNotExist(err) {
		t.Errorf("a stale copy in Schema/ is still embedded (stat err %v)", err)
	}
}

// One path prune cannot delete does not stop the rest, and is returned for
// the caller to judge. A non-empty directory stands in for a locked file,
// since os.Remove refuses both.
func TestRemoveStaleReturnsWhatItCannotDelete(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "locked/keep.txt", "x")
	writeUnder(t, dir, "gone/generated.go", generatedHeader)
	failed := removeStale(dir, []string{filepath.Join(dir, "locked"), filepath.Join(dir, "gone", "generated.go")})
	if len(failed) != 1 || !strings.HasSuffix(failed[0].path, "locked") || failed[0].err == nil {
		t.Errorf("failed = %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone", "generated.go")); !os.IsNotExist(err) {
		t.Errorf("a failure on one path stopped the rest (stat err %v)", err)
	}
}

// A stale SDL copy is embedded and keeps its types answering, so one that
// cannot be deleted fails the generate -- which a library caller with no
// Notef also hears. A non-empty directory under the copy's name is what the
// glob finds and os.Remove refuses on every OS.
func TestPruneFailsOnAnSDLCopyItCannotDelete(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	gen := func() error {
		return Generate(context.Background(), Config{Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph"})
	}
	if err := gen(); err != nil {
		t.Fatal(err)
	}
	writeUnder(t, dir, "graph/schema/legacy.graphql/keep.txt", "x")
	if err := gen(); err == nil || !strings.Contains(err.Error(), "still embedded") {
		t.Errorf("an undeletable SDL copy: err = %v", err)
	}
}

// A stale Go file is dead code that still compiles, so one that cannot be
// deleted is named in a note and the generate goes on to scaffold. A file
// held open is what Windows refuses to delete; elsewhere it is deleted and
// this skips.
func TestPruneNotesAGoFileItCannotDelete(t *testing.T) {
	dir := t.TempDir()
	writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
	writeUnder(t, dir, "sdl/legacy.graphql", "type Legacy { id: ID! }\nextend type Query { legacy: Legacy }\n")
	var notes []string
	gen := func() error {
		return Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph",
			Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
		})
	}
	if err := gen(); err != nil {
		t.Fatal(err)
	}
	staleGo := filepath.Join(dir, "graph", "legacy", "generated.go")
	held, err := os.Open(staleGo)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Remove(filepath.Join(dir, "sdl", "legacy.graphql")); err != nil {
		t.Fatal(err)
	}
	if err := gen(); err != nil {
		t.Fatalf("an undeletable Go file failed the generate: %v", err)
	}
	if _, statErr := os.Stat(staleGo); os.IsNotExist(statErr) {
		t.Skip("an open file is deletable here")
	}
	var noted bool
	for _, n := range notes {
		noted = noted || strings.Contains(n, filepath.Join("legacy", "generated.go"))
	}
	if !noted {
		t.Errorf("an undeletable Go file was not named: notes %q", notes)
	}
}

// Where a group package would land in a directory this run must not write --
// another gqlc run's root, another module, a file -- generation stops before
// writing and names the directory.
func TestGroupPackagesStayOutOfForeignDirectories(t *testing.T) {
	base := func(t *testing.T) string {
		dir := t.TempDir()
		writeUnder(t, dir, "sdl/query.graphql", "type Query { ping: Boolean }\n")
		writeUnder(t, dir, "sdl/admin.graphql", "type Admin { id: ID! }\nextend type Query { admin: Admin }\n")
		return dir
	}
	for name, c := range map[string]struct {
		setup    func(dir string)
		groupDir string
		want     string
	}{
		"group dir is another run's root": {func(dir string) {
			writeUnder(t, dir, "graph/admin/schema.go", generatedHeader+"\npackage admin\n")
			writeUnder(t, dir, "graph/admin/schema/admin.graphql", "type Query { a: Boolean }\n")
		}, "", "another gqlc run's output"},
		"group dir is another module": {func(dir string) {
			writeUnder(t, dir, "graph/admin/go.mod", "module example.com/admin\n")
		}, "", "another module"},
		"GroupDir reaches another module": {func(dir string) {
			writeUnder(t, dir, "graph/api/go.mod", "module example.com/api\n")
		}, "api", "another module"},
		"GroupDir crosses a file": {func(dir string) {
			writeUnder(t, dir, "graph/register", "not a directory")
		}, "register", "is a file"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := base(t)
			c.setup(dir)
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph", GroupDir: c.groupDir,
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// boundedEach visits every index once on at most ioParallel goroutines: a
// goroutine per item, parked on a semaphore, was thousands at 800 groups.
func TestBoundedEachUsesAFixedPool(t *testing.T) {
	const n = 500
	base := runtime.NumGoroutine()
	var seen [n]atomic.Int32
	var peak atomic.Int64
	boundedEach(n, func(i int) {
		seen[i].Add(1)
		if g := int64(runtime.NumGoroutine() - base); g > peak.Load() {
			peak.Store(g)
		}
		time.Sleep(50 * time.Microsecond)
	})
	for i := range n {
		if seen[i].Load() != 1 {
			t.Fatalf("index %d visited %d times", i, seen[i].Load())
		}
	}
	if peak.Load() > ioParallel {
		t.Errorf("%d goroutines at once, want at most %d", peak.Load(), ioParallel)
	}
}
