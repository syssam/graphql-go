package codegen

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The second Go type's package is imported under the base of its path, and a
// generated model package is named after its group. Nothing stopped the two
// from being the same word, and two imports under one qualifier is a
// redeclaration in a file marked DO NOT EDIT -- the same failure the
// generated/mapped collision was fixed for, reached from the other side.

const collideEnt = `package ent

import "hello/method"

type DepreciationMethod string

const (
	DepreciationMethodStraightLine DepreciationMethod = "straight_line"
	DepreciationMethodNone         DepreciationMethod = "none"
)

type MethodFilter struct {
	Method *method.Method
}

type Asset struct {
	ID     string
	Method DepreciationMethod
}
`

const collideMethodPkg = `package method

type Method string

const (
	MethodStraightLine Method = "straight_line"
	MethodNone         Method = "none"
)
`

// Two SDL files, so there are two groups; the first is named "method", which
// is also the base of the second enum type's package. Report is unbound so a
// model package is generated for the "method" group, which is what has to be
// imported beside hello/method.
const collideSDLMethod = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
input MethodFilter { method: Method }
type Report { id: ID! label: String! method: Method! }
type Query { report: Report! assets(filter: MethodFilter!): [Asset!]! }
`

const collideSDLAsset = `
type Asset { id: ID! method: Method! }
`

func TestSecondEnumPackageDoesNotCollideWithAGeneratedModelPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := t.TempDir()
	writeTempModule(t, dir)
	for name, sdl := range map[string]string{
		"method.graphql": collideSDLMethod,
		"asset.graphql":  collideSDLAsset,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sdl), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pkg, src := range map[string]string{
		"ent":    collideEnt,
		"method": collideMethodPkg,
	} {
		if err := os.MkdirAll(filepath.Join(dir, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pkg, pkg+".go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Generate(context.Background(), Config{
		Dir:            dir,
		SchemaGlobs:    []string{"*.graphql"},
		Output:         "graph",
		Package:        "hello/graph",
		AutoBind:       []string{"./ent"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	walk := func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		all.Write(b)
		return nil
	}
	if err := filepath.WalkDir(filepath.Join(dir, "graph"), walk); err != nil {
		t.Fatal(err)
	}
	src := all.String()
	if !strings.Contains(src, "hello/method") {
		t.Skipf("the second enum type was not bound, so there is no collision to test\n%s", src)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}
