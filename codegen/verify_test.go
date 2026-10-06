package codegen

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// verifyModule is a module with a struct the schema binds by name and no
// AutoBind, so the generator writes its fields from the SDL's names without
// reading it: the case where a generate exits 0 over code that does not
// compile. It is generated once and tidied, so that a build has what it needs.
func verifyModule(t *testing.T, field string) (string, Config) {
	t.Helper()
	dir := t.TempDir()
	writeTempModule(t, dir)
	writeFiles(t, dir, map[string]string{
		"schema.graphql": "type Server { slug: String! }\ntype Query { server: Server }\n",
		"svc/svc.go":     "package svc\n\ntype Server struct {\n\tSlug string\n\t" + field + " string\n}\n",
	})
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		Models: map[string]string{"Server": "hello/svc.Server"},
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	writeFiles(t, dir, map[string]string{
		"schema.graphql": "type Server { slug: String! url: String! }\ntype Query { server: Server }\n",
	})
	return dir, cfg
}

// Verify builds what was generated, so the generate that writes code that
// does not compile is the one that says so. Without it this one exits 0: url
// is written as v.Url against a field called URL.
func TestVerifyFailsAGenerateWhoseOutputDoesNotBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	_, cfg := verifyModule(t, "URL")
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatalf("without Verify the generate is expected to pass, which is the problem: %v", err)
	}
	cfg.Verify = true
	err := Generate(context.Background(), cfg)
	if err == nil {
		t.Fatal("Verify passed a generate whose output does not build")
	}
	for _, want := range []string{"codegen: verify:", "generated.go", "Url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
	// Nothing changed since, and the output is as broken as it was: a run
	// that writes nothing still builds, or the failure is reported once and
	// then called fixed.
	if err := Generate(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "codegen: verify:") {
		t.Errorf("a second generate over the same broken output said %v, want the same refusal", err)
	}
	// A check writes nothing and so builds nothing.
	cfg.Check = true
	if err := Generate(context.Background(), cfg); err != nil {
		t.Errorf("a check with Verify set said %v; the output is current, and building is a generate's", err)
	}
}

func TestVerifyPassesOutputThatBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	_, cfg := verifyModule(t, "Url")
	cfg.Verify = true
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatalf("Verify refused output that builds: %v", err)
	}
}
