package codegen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func orphanConfig(dir string, scaffold map[string]string) Config {
	return Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold,
	}
}

// A field dropped from the SDL leaves its method behind, and the method still
// compiles because the Resolver assertion checks only what the interface asks
// for. A generate says so in a note that scrolls away; CheckOptions.Orphans is
// the form a CI job can hold.
func TestCheckOrphansFailsOnAMethodNoResolverAsks(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	cfg := orphanConfig(dir, map[string]string{"user": "impl.UserResolver", "post": "impl.PostResolver"})
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// What is there after a clean generate is not an orphan, and neither is a
	// method nobody can call from outside the package.
	if err := Check(context.Background(), cfg, CheckOptions{Orphans: true}); err != nil {
		t.Fatalf("a fresh scaffold has an orphan: %v", err)
	}
	writeUnder(t, dir, "impl/extra.go", "package impl\n\nfunc (r *UserResolver) KpiDefinitions() bool { return false }\n\nfunc (r *UserResolver) helper() bool { return false }\n")

	err := Check(context.Background(), cfg, CheckOptions{Orphans: true})
	if !errors.Is(err, ErrOrphans) || errors.Is(err, ErrStale) || errors.Is(err, ErrStubs) {
		t.Fatalf("err = %v, want ErrOrphans alone", err)
	}
	if want := "KpiDefinitions (impl.UserResolver, impl/extra.go)"; !strings.Contains(err.Error(), want) {
		t.Errorf("the check does not name %q:\n%v", want, err)
	}
	for _, not := range []string{"helper", "Users", "Posts", "Author"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("the check names %s, which is live or unexported:\n%v", not, err)
		}
	}

	// Opt-in: the same tree is current without it, as the author's own exported
	// methods are legitimate on a type this check was not asked about.
	if err := Check(context.Background(), cfg, CheckOptions{}); err != nil {
		t.Fatalf("a check without Orphans: %v", err)
	}

	// Deleting the method clears it.
	if err := os.Remove(filepath.Join(dir, "impl", "extra.go")); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), cfg, CheckOptions{Orphans: true}); err != nil {
		t.Fatalf("after deleting the orphan: %v", err)
	}
}

// One type serving two groups has both groups' methods, and neither group's are
// orphans of the other; a method no group asks for is one, reported once.
func TestCheckOrphansJudgesASharedTypeByEveryGroupItServes(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	cfg := orphanConfig(dir, map[string]string{"user": "impl.Resolver", "post": "./impl/.Resolver"})
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), cfg, CheckOptions{Orphans: true}); err != nil {
		t.Fatalf("a live method of one group was called an orphan of the other: %v", err)
	}
	writeUnder(t, dir, "impl/extra.go", "package impl\n\nfunc (r *Resolver) Archived() bool { return false }\n")
	err := Check(context.Background(), cfg, CheckOptions{Orphans: true})
	if !errors.Is(err, ErrOrphans) {
		t.Fatalf("err = %v, want ErrOrphans", err)
	}
	if n := strings.Count(err.Error(), "Archived"); n != 1 {
		t.Errorf("Archived is named %d times, want once for the one type:\n%v", n, err)
	}
}

// Without a Scaffold there is no implementation to look in, so asking is a
// configuration error and not a check that passes over nothing.
func TestCheckOrphansNeedsScaffold(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	err := Check(context.Background(), orphanConfig(dir, nil), CheckOptions{Orphans: true})
	if err == nil || !strings.Contains(err.Error(), "Scaffold names none") {
		t.Fatalf("err = %v, want a refusal naming Scaffold", err)
	}
}
