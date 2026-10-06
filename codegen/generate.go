package codegen

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// ErrStale is what Generate returns, wrapped with the files concerned, when
// Config.Check finds that a generate would change what is on disk.
var ErrStale = errors.New("codegen: generated code is not current")

// Generate writes bindings, models, argument structs and a Resolver
// interface from SDL. It loads no Go packages unless AutoBind names some, and
// then only their export data.
func Generate(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cfg.SchemaGlobs) == 0 {
		return errors.New("codegen: SchemaGlobs is empty")
	}
	if cfg.Output == "" {
		return errors.New("codegen: Output is empty")
	}
	if cfg.Package == "" {
		return errors.New("codegen: Package is empty")
	}
	if err := checkGroupDir(cfg.GroupDir); err != nil {
		return err
	}
	if err := checkGoConfig(cfg); err != nil {
		return err
	}
	dir := cfg.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	outDir := outputDir(dir, cfg.Output)
	// Before the schema is loaded: a refused GroupDir costs nothing to report.
	if err := checkGroupDirRoot(outDir, cfg.GroupDir); err != nil {
		return err
	}
	b, err := newBuilder(dir, cfg)
	if err != nil {
		return err
	}
	if err := b.checkInline(); err != nil {
		return err
	}
	if err := b.expandScaffold(b.uniqueGroups()); err != nil {
		return err
	}
	// What the last generate left for this one. Read for what it says and
	// nothing else: whether the file is gqlc's to replace is asked below, of
	// every file.
	var recorded []string
	if last, err := os.ReadFile(filepath.Join(outDir, "schema.go")); err == nil {
		recorded, b.scalarsNamedBefore = recordedIn(last)
	}
	files, err := b.emit()
	if err != nil {
		return err
	}
	if err := b.checkGroupRoots(outDir); err != nil {
		return err
	}
	// Written concurrently: each write first reads the old file to leave an
	// unchanged one alone, and with a package per group that is thousands of
	// opens, which on Windows was most of a generate.
	rels := slices.Sorted(maps.Keys(files))
	errs := make([]error, len(rels))
	// Every refusal is found before anything is written, so a refused generate
	// leaves no package that is half this run's.
	olds := make([][]byte, len(rels))
	boundedEach(len(rels), func(i int) {
		olds[i], errs[i] = readExisting(filepath.Join(outDir, rels[i]))
	})
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := foreignSchemaFiles(outDir, files, recorded); err != nil {
		return err
	}
	// And what scaffolding would refuse, which came last, after the writes:
	// a misspelt scaffold key left a tree half this run's, and a check, which
	// stops here, called it current.
	if err := b.checkScaffold(b.uniqueGroups()); err != nil {
		return err
	}
	if err := b.checkScaffoldTargets(outDir, files); err != nil {
		return err
	}
	if cfg.Check {
		return b.check(outDir, rels, olds, files, recorded)
	}
	boundedEach(len(rels), func(i int) {
		errs[i] = writeGo(filepath.Join(outDir, rels[i]), olds[i], files[rels[i]])
	})
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := pruneGenerated(outDir, files, recorded, b.notef); err != nil {
		return err
	}
	if err := b.scaffold(b.uniqueGroups()); err != nil {
		return err
	}
	if cfg.Verify {
		return verifyBuild(ctx, dir, outDir)
	}
	return nil
}

// verifyBuild is Config.Verify: go build over every package under outDir,
// from dir, which is where the module is.
func verifyBuild(ctx context.Context, dir, outDir string) error {
	rel, err := filepath.Rel(dir, outDir)
	if err != nil {
		return fmt.Errorf("codegen: verify: %w", err)
	}
	cmd := exec.CommandContext(ctx, "go", "build", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if _, ran := errors.AsType[*exec.ExitError](err); !ran {
		return fmt.Errorf("codegen: verify: go build: %w", err)
	}
	// The first errors are the ones to read; a missing method is reported
	// again at every place the type is used.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	const shown = 20
	if len(lines) > shown {
		lines = append(lines[:shown], fmt.Sprintf("... and %d more lines", len(lines)-shown))
	}
	return fmt.Errorf("codegen: verify: the output is written and does not build:\n%s", strings.Join(lines, "\n"))
}

// checkGoConfig refuses the two settings that are written into generated code
// as they stand and may not be Go: the package name Package ends in, and the
// type each Models entry names. Left to the formatter they failed as a line
// and column in a file that was never written, followed by its source.
func checkGoConfig(cfg Config) error {
	name := cfg.Package
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if cfg.Package != "" && !token.IsIdentifier(name) {
		return fmt.Errorf("codegen: Package %q ends in %q, which is not a Go package name", cfg.Package, name)
	}
	var errs []error
	for _, gql := range slices.Sorted(maps.Keys(cfg.Models)) {
		if expr := cfg.Models[gql]; !modelExprOK(expr) {
			errs = append(errs, fmt.Errorf("codegen: Models[%q] = %q is not a Go type, written as import/path.Type; a type built over another package's, such as a map or a generic, needs a named type of its own to map", gql, expr))
		}
	}
	if !cfg.Wire.IsZero() {
		if path, _ := splitModelExpr(cfg.Wire.Deps); path == "" || strings.TrimLeft(cfg.Wire.Deps, "*[]") != cfg.Wire.Deps || !modelExprOK(cfg.Wire.Deps) {
			errs = append(errs, fmt.Errorf("codegen: Wire.Deps = %q is not a Go type written as import/path.Type", cfg.Wire.Deps))
		}
	} else if len(cfg.Wire.Pass) > 0 {
		errs = append(errs, errors.New("codegen: Wire.Pass says what constructors are given, and without Wire.Deps there is no Wire to give it"))
	}
	return errors.Join(errs...)
}

// outputDir is where Output is on disk: under dir when it is relative, and
// where it says when it is absolute, which Join would have put under dir too.
func outputDir(dir, output string) string {
	output = filepath.FromSlash(output)
	if filepath.IsAbs(output) {
		return output
	}
	return filepath.Join(dir, output)
}
