package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An ent-derived ORM generates one Go enum type beside the entity and another
// beside the column, and its filter inputs carry the second. @goModel names
// the first, so the second is bound by nothing and every input field using it
// is a NewSchema error the SDL gives no hint of. Discovery finds it through
// the bound input struct, loads the package it lives in -- which no AutoBind
// pattern and no directive named -- and binds it as a second Go type for the
// same SDL enum.

const altEnumSDL = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
input MethodFilter { method: Method }
type Asset { id: ID! method: Method! }
type Query { assets(filter: MethodFilter!): [Asset!]! }
`

const altEnumEnt = `package ent

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

const altEnumPkg = `package method

type Method string

const (
	MethodStraightLine Method = "straight_line"
	MethodNone         Method = "none"
)
`

func writeAltEnumModule(t *testing.T) string {
	t.Helper()
	dir := writeEntModule(t, altEnumSDL, altEnumEnt)
	if err := os.MkdirAll(filepath.Join(dir, "method"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "method", "method.go"), []byte(altEnumPkg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAutoBindDiscoversASecondGoTypeForAnEnum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeAltEnumModule(t)
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	})

	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{
		`graphql.Enum[ent.DepreciationMethod]("Method"`,
		`graphql.Enum[method.Method]("Method"`,
		`method.MethodStraightLine: "STRAIGHT_LINE"`,
		`method.MethodNone: "NONE"`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated code does not contain %s\n%s", want, src)
		}
	}
	// The package holding the second type is named by nothing in the config,
	// so its import has to come from the same place the reference does.
	if !strings.Contains(src, `"hello/method"`) {
		t.Errorf("the second enum's package was not imported\n%s", src)
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

// The bar is the one a first binding is held to: a map missing a value would
// leave that value unrepresentable at every position holding this type, which
// is a wrong answer at run time where the unbound type is a refused build.
func TestPartialSecondTypeBindsNothingAndSaysSo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeAltEnumModule(t)
	partial := strings.Replace(altEnumPkg, "\tMethodNone         Method = \"none\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "method", "method.go"), []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	var notes []string
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		Notef:          func(f string, a ...any) { notes = append(notes, f) },
	})
	if strings.Contains(src, "graphql.Enum[method.Method]") {
		t.Errorf("bound a second type whose NONE has no constant\n%s", src)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "second Go type") {
			found = true
		}
	}
	if !found {
		t.Errorf("nothing was reported about the unbound second type: %v", notes)
	}
}
