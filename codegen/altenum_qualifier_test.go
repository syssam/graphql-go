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

// extraQualifier settles the name an alternate enum's package is imported
// under, and it has three reachable branches that only the real consumer
// schema had ever exercised. One of them is the fix for a bug that schema
// found -- a package Config.Models already imports being imported a second
// time, which is a redeclaration in a file marked DO NOT EDIT -- and it was
// verified by regenerating against that schema rather than by a test here.
// That is the same "checked against something not in the repository" gap this
// review has been closing elsewhere.

const qualSDL = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
enum Grade  @goModel(model: "hello/ent.GradeKind") { HIGH LOW }
input Filters { method: Method grade: Grade }
type Asset { id: ID! method: Method! grade: Grade! }
type Query { assets(filter: Filters!): [Asset!]! }
`

const qualEnt = `package ent

import "hello/shared"

type DepreciationMethod string

const (
	DepreciationMethodStraightLine DepreciationMethod = "straight_line"
	DepreciationMethodNone         DepreciationMethod = "none"
)

type GradeKind string

const (
	GradeKindHigh GradeKind = "high"
	GradeKindLow  GradeKind = "low"
)

type Filters struct {
	Method *shared.Method
	Grade  *shared.Grade
}

type Asset struct {
	ID     string
	Method DepreciationMethod
	Grade  GradeKind
}
`

// Both alternate types live in one package, so discovery must import it once
// and refer to both through the same qualifier.
const qualShared = `package shared

type Method string

const (
	MethodStraightLine Method = "straight_line"
	MethodNone         Method = "none"
)

type Grade string

const (
	GradeHigh Grade = "high"
	GradeLow  Grade = "low"
)
`

func writeQualModule(t *testing.T, sdl, entSrc string, extra map[string]string) string {
	t.Helper()
	dir := writeEntModule(t, sdl, entSrc)
	for pkg, src := range extra {
		if err := os.MkdirAll(filepath.Join(dir, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
		name := pkg[strings.LastIndex(pkg, "/")+1:]
		if err := os.WriteFile(filepath.Join(dir, pkg, name+".go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// generatedSources concatenates every generated .go file, which is what has to
// compile as a whole.
func generatedSources(t *testing.T, dir string) string {
	t.Helper()
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
	return all.String()
}

func buildGenerated(t *testing.T, dir string) {
	t.Helper()
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

// Two enums, two alternate types, one package between them: imported once.
func TestTwoAlternateEnumsInOnePackageShareOneImport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeQualModule(t, qualSDL, qualEnt, map[string]string{"shared": qualShared})
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		AutoBind:       []string{"./ent"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Fatal(err)
	}
	src := generatedSources(t, dir)
	if !strings.Contains(src, "shared.Method") || !strings.Contains(src, "shared.Grade") {
		t.Fatalf("both alternate types must be bound through one qualifier:\n%s", src)
	}
	if n := strings.Count(src, `"hello/shared"`); n != 1 {
		t.Errorf(`"hello/shared" is imported %d times in one file set; one package is `+
			`one import, and a second is a redeclaration`, n)
	}
	buildGenerated(t, dir)
}

// The alternate package is one Config.Models already names, so it is already
// imported: reusing that qualifier rather than registering a second import is
// the fix for the redeclaration the real schema produced.
func TestAnAlternatePackageAlreadyNamedByModelsIsNotImportedTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
scalar Ref
input Filters { method: Method }
type Asset { id: ID! method: Method! ref: Ref! }
type Query { assets(filter: Filters!): [Asset!]! }
`
	const entSrc = `package ent

import "hello/shared"

type DepreciationMethod string

const (
	DepreciationMethodStraightLine DepreciationMethod = "straight_line"
	DepreciationMethodNone         DepreciationMethod = "none"
)

type Filters struct {
	Method *shared.Method
}

type Asset struct {
	ID     string
	Method DepreciationMethod
	Ref    shared.Ref
}
`
	const sharedSrc = `package shared

type Ref string

type Method string

const (
	MethodStraightLine Method = "straight_line"
	MethodNone         Method = "none"
)
`
	dir := writeQualModule(t, sdl, entSrc, map[string]string{"shared": sharedSrc})
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		AutoBind:       []string{"./ent"},
		Models:         map[string]string{"Ref": "hello/shared.Ref"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Fatal(err)
	}
	src := generatedSources(t, dir)
	if n := strings.Count(src, `"hello/shared"`); n > 1 {
		t.Errorf(`"hello/shared" is imported %d times; Models already imports it, so the `+
			`alternate enum must reuse that qualifier rather than add a second import`, n)
	}
	buildGenerated(t, dir)
}

// And when the base name is taken by a *different* package, both have to stay
// referable, so the alternate one takes a suffixed qualifier.
func TestAnAlternatePackageWhoseBaseNameIsTakenGetsASuffix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
scalar Ref
input Filters { method: Method }
type Asset { id: ID! method: Method! ref: Ref! }
type Query { assets(filter: Filters!): [Asset!]! }
`
	const entSrc = `package ent

import (
	one "hello/one/shared"
	two "hello/two/shared"
)

type DepreciationMethod string

const (
	DepreciationMethodStraightLine DepreciationMethod = "straight_line"
	DepreciationMethodNone         DepreciationMethod = "none"
)

type Filters struct {
	Method *two.Method
}

type Asset struct {
	ID     string
	Method DepreciationMethod
	Ref    one.Ref
}
`
	dir := writeQualModule(t, sdl, entSrc, map[string]string{
		"one/shared": "package shared\n\ntype Ref string\n",
		"two/shared": `package shared

type Method string

const (
	MethodStraightLine Method = "straight_line"
	MethodNone         Method = "none"
)
`,
	})
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		AutoBind:       []string{"./ent"},
		Models:         map[string]string{"Ref": "hello/one/shared.Ref"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Fatal(err)
	}
	// Compiling is the assertion: two packages whose base name is "shared"
	// cannot both be referred to as `shared`, so if the suffix were missing
	// this would not build.
	buildGenerated(t, dir)
}
