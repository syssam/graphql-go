package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule lays out a module with an SDL file, so the flag form has a go.mod
// to derive the output package from -- which is the whole point of the form.
func writeModule(t *testing.T, modulePath, sdl string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const flagSDL = "type User { id: ID! name: String! }\ntype Query { users: [User!]! }\n"

// chdir keeps the flag form honest: Dir is the process working directory, so a
// test that passed absolute paths would not be exercising the documented usage.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestFlagsNeedNoConfigFile(t *testing.T) {
	dir := writeModule(t, "example.com/app", flagSDL)
	chdir(t, dir)

	if err := run([]string{"-schema", "schema.graphql", "-out", "graph"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatalf("generated.go: %v", err)
	}
	// The package was never passed: it has to have come from go.mod.
	if got := string(b); !strings.Contains(got, "example.com/app/graph") &&
		!strings.Contains(got, "package graph") {
		t.Fatalf("generated output does not look like the derived package:\n%s", got[:min(400, len(got))])
	}
	if _, err := os.Stat(filepath.Join(dir, "gqlc.yaml")); !os.IsNotExist(err) {
		t.Fatal("the flag form must not need or write a gqlc.yaml")
	}
}

func TestFlagsRejectConfigTogetherWithSchema(t *testing.T) {
	dir := writeModule(t, "example.com/app", flagSDL)
	chdir(t, dir)

	err := run([]string{"-config", "gqlc.yaml", "-schema", "schema.graphql", "-out", "graph"})
	if err == nil {
		t.Fatal("want an error when both forms are given")
	}
	if !strings.Contains(err.Error(), "pass one") {
		t.Fatalf("error = %v, want it to say to pass one", err)
	}
}

func TestFlagsRequireOut(t *testing.T) {
	dir := writeModule(t, "example.com/app", flagSDL)
	chdir(t, dir)

	err := run([]string{"-schema", "schema.graphql"})
	if err == nil || !strings.Contains(err.Error(), "-out is required") {
		t.Fatalf("error = %v, want it to require -out", err)
	}
}

func TestDerivePackageWithoutGoMod(t *testing.T) {
	// A directory with no go.mod above it cannot have its package derived, and
	// the error has to say what to do instead rather than name a stat failure.
	dir := t.TempDir()
	root := filepath.VolumeName(dir) + string(filepath.Separator)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		t.Skip("a go.mod at the filesystem root would make this derivable")
	}
	_, err := derivePackage(filepath.Join(dir, "graph"))
	if err == nil || !strings.Contains(err.Error(), "-pkg") {
		t.Fatalf("error = %v, want it to name -pkg", err)
	}
}

func TestDerivePackageJoinsTheRelativePath(t *testing.T) {
	dir := writeModule(t, "example.com/app", flagSDL)
	got, err := derivePackage(filepath.Join(dir, "internal", "graph"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "example.com/app/internal/graph"; got != want {
		t.Fatalf("derivePackage = %q, want %q", got, want)
	}
	// The module root itself is the module path, with no trailing slash.
	if got, err = derivePackage(dir); err != nil || got != "example.com/app" {
		t.Fatalf("derivePackage(root) = %q, %v", got, err)
	}
}

func TestRepeatableAndModels(t *testing.T) {
	var r repeatable
	for _, v := range []string{"a.graphql,b.graphql", " c.graphql "} {
		if err := r.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := strings.Join(r, "|"), "a.graphql|b.graphql|c.graphql"; got != want {
		t.Fatalf("repeatable = %q, want %q", got, want)
	}

	// The separator is the first =, and the import path keeps every dot it has.
	m, err := parseModels([]string{"Time=gopkg.in/x.Time", "UUID=github.com/g/uuid.UUID"})
	if err != nil {
		t.Fatal(err)
	}
	if m["Time"] != "gopkg.in/x.Time" || m["UUID"] != "github.com/g/uuid.UUID" {
		t.Fatalf("parseModels = %v", m)
	}
	for _, bad := range []string{"NoEquals", "=go/x.T", "Name="} {
		if _, err := parseModels([]string{bad}); err == nil {
			t.Fatalf("parseModels(%q) = nil error, want one", bad)
		}
	}
}

// TestFlagsAndConfigAgree is the claim this change actually makes: the flag
// form is another way to say the same thing, not a second generator.
//
// It compares every generated file, not generated.go alone. The first version
// read only generated.go and could not fail: codegen emits four files and
// nullableInputOmittable only reaches model/models.go, so breaking that flag on
// the flag path left the one file under test byte-identical.
func TestFlagsAndConfigAgree(t *testing.T) {
	const sdl = `
scalar Time
enum Role { ADMIN USER }
input Filter { name: String, since: Time }
type User { id: ID! name: String! role: Role! joined: Time! }
type Query { users(filter: Filter): [User!]! }
`
	generated := func(t *testing.T, dir string) map[string]string {
		t.Helper()
		out := map[string]string{}
		root := filepath.Join(dir, "graph")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) < 2 {
			t.Fatalf("only %d generated file(s) under %s; the comparison would prove little", len(out), root)
		}
		return out
	}

	diff := func(a, b map[string]string) string {
		var msg []string
		for name, want := range a {
			got, ok := b[name]
			switch {
			case !ok:
				msg = append(msg, "missing: "+name)
			case got != want:
				msg = append(msg, "differs: "+name)
			}
		}
		for name := range b {
			if _, ok := a[name]; !ok {
				msg = append(msg, "unexpected: "+name)
			}
		}
		return strings.Join(msg, "; ")
	}

	viaConfig := writeModule(t, "example.com/app", sdl)
	cfg := `schema: [schema.graphql]
output: graph
package: example.com/app/graph
nullableInputOmittable: true
models:
  Time: time.Time
`
	if err := os.WriteFile(filepath.Join(viaConfig, "gqlc.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, viaConfig)
	if err := run([]string{"-config", "gqlc.yaml"}); err != nil {
		t.Fatalf("config form: %v", err)
	}
	want := generated(t, viaConfig)

	// A positive control: without this, every option could be silently ignored
	// on both paths and the comparison would still pass.
	var sawOmittable bool
	for _, body := range want {
		sawOmittable = sawOmittable || strings.Contains(body, "Omittable")
	}
	if !sawOmittable {
		t.Fatal("nullableInputOmittable produced no Omittable anywhere; the SDL does not exercise the option")
	}

	flagArgs := []string{
		"-schema", "schema.graphql",
		"-out", "graph",
		"-model", "Time=time.Time",
		"-nullable-input-omittable",
	}
	viaFlags := writeModule(t, "example.com/app", sdl)
	chdir(t, viaFlags)
	if err := run(flagArgs); err != nil {
		t.Fatalf("flag form: %v", err)
	}
	if d := diff(want, generated(t, viaFlags)); d != "" {
		t.Fatalf("the two forms disagree: %s", d)
	}

	// -model has to be read, not merely accepted: a wrong Go type must change
	// the output, or the flag could be unwired and this test would not know.
	wrong := writeModule(t, "example.com/app", sdl)
	chdir(t, wrong)
	if err := run([]string{"-schema", "schema.graphql", "-out", "graph", "-model", "Time=time.time", "-nullable-input-omittable"}); err != nil {
		t.Fatalf("flag form: %v", err)
	}
	if d := diff(want, generated(t, wrong)); d == "" {
		t.Fatal("-model was not applied: a wrong Go type produced identical output")
	}
}
