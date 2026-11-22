package codegen

import (
	"context"
	"errors"
	"fmt"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
)

// A context cancelled during a generate, once the schema is loaded and
// packages are bound, stops it with the context's error before anything is
// written; Render stops the same way.
func TestCancelDuringGenerateStopsBeforeWriting(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeEntModule(t, autoSDL, entSource)
	before := snapshot(t, dir)
	// GroupFunc is asked while the files are produced, after AutoBind has
	// loaded its packages: caller code running mid-generate.
	var cancel context.CancelFunc
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		AutoBind:  []string{"./ent"},
		GroupFunc: func(string, string) string { cancel(); return "" },
	}
	renderCtx, cancelRender := context.WithCancel(context.Background())
	defer cancelRender()
	cancel = cancelRender
	if _, err := Render(renderCtx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Render: err = %v, want context.Canceled", err)
	}
	genCtx, cancelGen := context.WithCancel(context.Background())
	defer cancelGen()
	cancel = cancelGen
	if err := Generate(genCtx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate: err = %v, want context.Canceled", err)
	}
	if after := snapshot(t, dir); !maps.Equal(before, after) {
		t.Errorf("a cancelled generate changed the tree")
	}
}

// The context reaches packages.Load itself: an already-cancelled one fails
// the load, rather than the load running to the end and the cancellation
// being noticed after.
func TestAutoBindLoadsUnderTheContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTempModule(t, dir)
	b := &builder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := autoBind(ctx, dir, []string{"./..."}, nil, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A failed load of the alternate enum packages is not a refusal, but it is
// said: the alternates go unbound, and the author is told which packages and
// why. A cancelled context is not a failure to report.
func TestAltEnumLoadFailureIsNoted(t *testing.T) {
	t.Parallel()
	named := types.NewNamed(types.NewTypeName(0, types.NewPackage("example.com/alt", "alt"), "Status", nil), types.Typ[types.String], nil)
	alt := map[string][]*types.Named{"Status": {named}}
	missing := filepath.Join(t.TempDir(), "not-there")

	var notes []string
	notef := func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) }
	var loads int
	if pkgs := altEnumPkgs(context.Background(), missing, alt, nil, &loads, notef); pkgs != nil {
		t.Fatalf("a load from a missing directory returned %d packages", len(pkgs))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "example.com/alt") || !strings.Contains(notes[0], "not bound") {
		t.Fatalf("notes = %q, want one naming example.com/alt", notes)
	}

	notes = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	altEnumPkgs(ctx, missing, alt, nil, &loads, notef)
	if len(notes) != 0 {
		t.Errorf("a cancelled load was reported as a failure: %q", notes)
	}
}

// The context reaches the go command each packages.Load runs, not only the
// checks around it: a go that never answers is stopped when the deadline
// passes, where without the context the load waits for it. modelPkgs and
// altEnumPkgs swallow a failed load by design, so for them returning at all
// is the observable, and each is driven on its own -- autoBind's ctx check
// after its first load would otherwise hide either one losing its context.
// The fake go is a shell script, so this runs where sh does.
func TestEveryLoadStopsAHungGoCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake go command is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	enum := &ast.Schema{Types: map[string]*ast.Definition{"Status": {Kind: ast.Enum, Name: "Status"}}}
	alt := map[string][]*types.Named{"Status": {
		types.NewNamed(types.NewTypeName(0, types.NewPackage("example.com/alt", "alt"), "Status", nil), types.Typ[types.String], nil),
	}}
	for name, load := range map[string]func(context.Context, string) error{
		"autoBind": func(ctx context.Context, dir string) error {
			_, err := autoBind(ctx, dir, []string{"./..."}, nil, &builder{})
			return err
		},
		"modelPkgs": func(ctx context.Context, dir string) error {
			var loads int
			modelPkgs(ctx, dir, enum, map[string]string{"Status": "example.com/m.Status"}, nil, &loads)
			if loads != 1 {
				return fmt.Errorf("loads = %d, want the one load this test is about", loads)
			}
			return ctx.Err()
		},
		"altEnumPkgs": func(ctx context.Context, dir string) error {
			var loads int
			altEnumPkgs(ctx, dir, alt, nil, &loads, func(string, ...any) {})
			if loads != 1 {
				return fmt.Errorf("loads = %d, want the one load this test is about", loads)
			}
			return ctx.Err()
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		err := load(ctx, t.TempDir())
		took := time.Since(start)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: err = %v, want context.DeadlineExceeded", name, err)
		}
		if took > 10*time.Second {
			t.Errorf("%s: the load took %v: the hung go command was not stopped", name, took)
		}
	}
}
