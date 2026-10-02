package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

// The help lists every key gqlc.yaml takes, nested ones too, and every flag,
// within 80 columns. It used to list the seven flags and nothing else, so
// preset, scaffold, wire and the rest were found by reading the README or
// not at all; this fails the moment a key or a flag is added without a line.
func TestHelpNamesEveryKeyAndFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runTo([]string{"-h"}, &out, &out); err != nil {
		t.Fatalf("-h is a request for help, not an error: %v", err)
	}
	help := out.String()
	for _, want := range []string{"Usage:", "gqlc -schema", "gqlc -check", "gqlc -version"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not show %s", want)
		}
	}
	// A key opens its line: at two spaces when it is top-level, at four as
	// parent.child when it is nested.
	hasLine := func(indent, key string) bool {
		return strings.Contains(help, "\n"+indent+key+" ") || strings.Contains(help, "\n"+indent+key+"\n")
	}
	keys := 0
	typ := reflect.TypeFor[fileConfig]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if key == "" {
			t.Fatalf("fileConfig.%s has no yaml key, so this test cannot look for it", f.Name)
		}
		keys++
		if !hasLine("  ", key) {
			t.Errorf("the help has no line for the gqlc.yaml key %s", key)
		}
		if f.Type.Kind() != reflect.Struct {
			continue
		}
		for j := range f.Type.NumField() {
			sub, _, _ := strings.Cut(f.Type.Field(j).Tag.Get("yaml"), ",")
			if sub == "" {
				t.Fatalf("fileConfig.%s.%s has no yaml key", f.Name, f.Type.Field(j).Name)
			}
			keys++
			if !hasLine("    ", key+"."+sub) {
				t.Errorf("the help has no line for the gqlc.yaml key %s.%s", key, sub)
			}
		}
	}
	if keys < 30 {
		t.Fatalf("only %d keys found by reflection; the test is not reading fileConfig", keys)
	}
	fs, _ := newFlags(&bytes.Buffer{})
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(help, "\n  -"+f.Name+" ") && !strings.Contains(help, "\n  -"+f.Name+"\n") {
			t.Errorf("the help does not list the flag -%s", f.Name)
		}
	})
	for i, line := range strings.Split(strings.ReplaceAll(help, "\t", "        "), "\n") {
		if len([]rune(line)) > 80 {
			t.Errorf("line %d is %d columns: %q", i+1, len([]rune(line)), line)
		}
	}
}

// What -version prints, from build information as each way of building
// leaves it.
func TestFormatVersion(t *testing.T) {
	main := func(version string, replace *debug.Module, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Path: "github.com/syssam/graphql-go", Version: version, Replace: replace}, Settings: settings}
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "05f0bb43786e2c1a9f"}
	for name, c := range map[string]struct {
		bi   *debug.BuildInfo
		want string
	}{
		"a released version, as go tool in a project requiring it": {main("v1.2.3", nil), "gqlc v1.2.3 go1.27.1"},
		// go tool where go.mod replaces graphql-go with a checkout.
		"a replaced version": {main("v0.0.0-20260926154251-135bb6be891a", &debug.Module{Path: "../..", Version: "(devel)"}),
			"gqlc v0.0.0-20260926154251-135bb6be891a => ../.. (devel) go1.27.1"},
		// go build in a checkout: the pseudo-version has the commit already.
		"a build from a checkout": {main("v0.0.0-20261002212328-05f0bb43786e+dirty", nil, rev, debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			"gqlc v0.0.0-20261002212328-05f0bb43786e+dirty go1.27.1"},
		// A build that has no version but does have the commit.
		"a devel build with its commit": {main("(devel)", nil, rev, debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			"gqlc (devel) 05f0bb43786e+dirty go1.27.1"},
		"go run, with nothing to say": {main("(devel)", nil), "gqlc (devel) go1.27.1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := formatVersion(c.bi); got != c.want {
				t.Errorf("formatVersion = %q\nwant %q", got, c.want)
			}
		})
	}
}

// What the command prints goes where it was told to, which is what lets a
// test, or a tool embedding gqlc, read it.
func TestOutputGoesWhereItIsSent(t *testing.T) {
	var stderr bytes.Buffer
	if err := runTo([]string{"-nosuchflag"}, &bytes.Buffer{}, &stderr); err == nil || !strings.Contains(stderr.String(), "nosuchflag") {
		t.Errorf("an unknown flag: err %v, stderr %q", err, stderr.String())
	}
	if err := runTo([]string{"-version", "stray"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stray") {
		t.Errorf("-version with a stray argument: %v", err)
	}
	dir := t.TempDir()
	// A custom scalar mapped by models is one of the notes a generate makes.
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte("scalar Time\ntype Query { now: Time! }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gqlc.yaml")
	if err := os.WriteFile(cfg, []byte("schema: [schema.graphql]\noutput: graph\npackage: example/graph\nmodels: {Time: time.Time}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := runTo([]string{"-config", cfg}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "custom scalar") {
		t.Errorf("the note did not reach the stderr given: %q", stderr.String())
	}
	// And a check that passes says nothing at all.
	var quiet bytes.Buffer
	if err := runTo([]string{"-config", cfg, "-check"}, &quiet, &quiet); err != nil {
		t.Fatal(err)
	}
	if quiet.Len() != 0 {
		t.Errorf("a passing check printed %q", quiet.String())
	}
}

func TestVersionSaysWhichGqlc(t *testing.T) {
	var out bytes.Buffer
	if err := runTo([]string{"-version"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "gqlc ") || !strings.Contains(got, "go1.") {
		t.Errorf("-version printed %q, want the version and the Go it was built with", got)
	}
}

// -check generates in memory, writes nothing, and fails naming what is out
// of date: the "is the generated code current" step a CI job and a
// pre-commit hook want, without running go generate and asking git.
func TestCheckFlag(t *testing.T) {
	dir := t.TempDir()
	sdl := filepath.Join(dir, "schema.graphql")
	if err := os.WriteFile(sdl, []byte("type User { id: ID! }\ntype Query { users: [User!]! }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gqlc.yaml")
	if err := os.WriteFile(cfg, []byte("schema: [schema.graphql]\noutput: graph\npackage: example/graph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTo([]string{"-config", cfg, "-check"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "create graph/generated.go") {
		t.Fatalf("nothing generated yet, and -check said %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph")); err == nil {
		t.Fatal("-check wrote the output")
	}
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	if err := runTo([]string{"-config", cfg, "-check"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("just generated, and -check said %v", err)
	}
	if err := os.WriteFile(sdl, []byte("type User { id: ID! name: String }\ntype Query { users: [User!]! }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runTo([]string{"-config", cfg, "-check"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not current") || !strings.Contains(err.Error(), "rewrite graph/") {
		t.Fatalf("the schema changed, and -check said %v", err)
	}

	// The flag form takes it too.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := runTo([]string{"-schema", "schema.graphql", "-out", "graph", "-check"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not current") {
		t.Fatalf("the flag form's -check said %v", err)
	}
}
