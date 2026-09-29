package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GroupDir moves every group package under one directory, and the root, the
// scaffolded resolvers and the old layout's files all have to follow: an
// import still naming graph/user does not build, and a graph/user left beside
// graph/register/user is a second, dead copy of the group. The resolvers are
// scaffolded inside Output, where pruning runs, and must survive it.
func TestGroupDirHoldsEveryGroupPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, _ := scaffoldModule(t, nil)
	generate := func(groupDir string, scaffold map[string]string) {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph",
			GroupDir: groupDir, Scaffold: scaffold,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The layout a project moves from. Its hand-written code importing
	// graph/user would be the author's to change; the generated files are
	// the generator's, and those are what this checks.
	generate("", nil)
	if _, err := os.Stat(filepath.Join(dir, "graph", "user", "generated.go")); err != nil {
		t.Fatalf("without GroupDir the group should be graph/user: %v", err)
	}
	// Both groups implemented in one package, as a resolver layer in front
	// of per-entity services is.
	generate("register", map[string]string{"user": "graph/resolver.UserResolver", "post": "graph/resolver.PostResolver"})

	for _, g := range []string{"user", "post"} {
		if _, err := os.Stat(filepath.Join(dir, "graph", "register", g, "generated.go")); err != nil {
			t.Errorf("group %s is not under graph/register: %v", g, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "graph", g)); !os.IsNotExist(err) {
			t.Errorf("graph/%s outlived the move to graph/register (stat err %v)", g, err)
		}
	}
	if root := readFile(t, filepath.Join(dir, "graph", "schema.go")); !strings.Contains(root, `"hello/graph/register/user"`) {
		t.Errorf("schema.go does not import the moved group:\n%s", root)
	}
	resolverFile := filepath.Join(dir, "graph", "resolver", "user.resolvers.go")
	res := readFile(t, resolverFile)
	if !strings.Contains(res, `"hello/graph/register/user"`) {
		t.Errorf("the scaffolded resolver does not import the moved group:\n%s", res)
	}
	// Once written, the resolver is the author's. A second run prunes Output
	// again and must leave it -- and the package it is in -- alone.
	written := strings.Replace(res, `panic("not implemented: Query.users")`, `return nil, nil // written by hand`, 1)
	if written == res {
		t.Fatalf("no Query.users stub to fill in:\n%s", res)
	}
	if err := os.WriteFile(resolverFile, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	generate("register", map[string]string{"user": "graph/resolver.UserResolver", "post": "graph/resolver.PostResolver"})
	if got := readFile(t, resolverFile); got != written {
		t.Errorf("a regenerate changed the hand-written resolver:\n%s", got)
	}
	goBuild(t, dir)
}

func TestGroupDirMustStayInsideOutput(t *testing.T) {
	dir, _ := scaffoldModule(t, nil)
	// "Model" and "SCHEMA/x" are the reserved directories on Windows and macOS,
	// whose filesystems ignore case.
	// And names the go command ignores, where pruning is skipped too.
	for _, bad := range []string{"..", "../elsewhere", "a/../../b", ".", "model", "model/x", "schema", "/abs", "Model", "SCHEMA/x",
		"testdata", "gen/vendor", "_gen", ".gen",
		// Not importable from outside Output, and Windows' drive-relative form.
		"internal", "gen/Internal", "C:foo",
		// Not an import path element on any OS.
		"gen:x", "my gen", "gen@v2",
		// Not held as spelled on Windows: a trailing dot, a device name, a short name.
		"reg.", "con", "gen/NUL.x", "gen~1"} {
		err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph", GroupDir: bad,
		})
		if err == nil || !strings.Contains(err.Error(), "GroupDir") {
			t.Errorf("GroupDir %q: err = %v", bad, err)
		}
	}
	for _, good := range []string{"register", "gen/register"} {
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph", GroupDir: good,
		}); err != nil {
			t.Errorf("GroupDir %q: %v", good, err)
		}
	}
}
