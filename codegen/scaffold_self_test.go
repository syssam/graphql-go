package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A group's resolver can live in the group's own generated package, beside
// generated.go, so one entity is one package. There the group is not imported
// -- it is the package -- and nothing is qualified by it: importing itself is
// a cycle the compiler refuses.
func TestScaffoldIntoItsOwnGroupPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := scaffoldModule(t, map[string]string{
		"user": "graph/user.Handler",
		"post": "graph/post.Handler",
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	user := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
	for _, want := range []string{"package user", "type Handler struct{}", "var _ Resolver = (*Handler)(nil)"} {
		if !strings.Contains(user, want) {
			t.Errorf("user.resolvers.go lacks %q\n%s", want, user)
		}
	}
	post := readFile(t, filepath.Join(dir, "graph", "post", "post.resolvers.go"))
	for _, bad := range []string{`"hello/graph/post"`, "postgql."} {
		if strings.Contains(post, bad) {
			t.Errorf("post.resolvers.go refers to its own package as %q\n%s", bad, post)
		}
	}
	if !strings.Contains(post, "args PostArgs") {
		t.Errorf("post.resolvers.go does not name its args unqualified\n%s", post)
	}
	// A second run finds every method in the package it scaffolded into, and
	// the generated file beside it is left to the generator.
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if again := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); again != user {
		t.Errorf("a second run changed the resolver:\n%s", again)
	}
	goBuild(t, dir)
}

// In its own group package the implementation shares a namespace with the
// generated code, so a name the generator declares is refused by name, not
// left to a redeclaration inside a file marked DO NOT EDIT.
func TestScaffoldRefusesAGeneratedNameInItsOwnPackage(t *testing.T) {
	// Its own package, and another group's: either way the name is taken by
	// code the author cannot edit.
	for _, target := range []string{"graph/post.Resolver", "graph/post.PostArgs", "graph/post.Bindings", "graph/user.Resolver", "graph/user.Bindings"} {
		_, generate := scaffoldModule(t, map[string]string{"post": target})
		err := generate()
		typ := target[strings.LastIndex(target, ".")+1:]
		if err == nil || !strings.Contains(err.Error(), typ) || !strings.Contains(err.Error(), "generated") {
			t.Errorf("target %s: err = %v", target, err)
		}
	}
}

// The group's own directory spelled with other letter case is still the
// group's own package on a filesystem that ignores case -- Windows, macOS --
// so it must not import itself. Skipped where case matters, since there the
// two spellings are two directories.
func TestScaffoldOwnPackageInOtherLetterCase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := scaffoldModule(t, map[string]string{"user": "Graph/user.Handler"})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "GRAPH")); err != nil {
		t.Skip("case-sensitive filesystem: Graph and graph are different directories")
	}
	user := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
	if strings.Contains(user, `"hello/graph/user"`) || strings.Contains(user, "usergql.") {
		t.Errorf("the resolver imports its own package:\n%s", user)
	}
	goBuild(t, dir)
}
