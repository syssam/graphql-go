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

// ErrStale is what Check returns, wrapped with the files concerned, when
// Check finds that a generate would change what is on disk.
var ErrStale = errors.New("codegen: generated code is not current")

// ErrStubs is what Check returns, wrapped with the methods concerned, when
// Check with CheckOptions.Stubs finds a scaffolded stub left unimplemented.
var ErrStubs = errors.New("codegen: scaffolded Resolver methods are not implemented")

// ErrOrphans is what Check returns, wrapped with the methods concerned, when
// Check with CheckOptions.Orphans finds an exported method on a Scaffold
// target that none of the Resolvers it implements has.
var ErrOrphans = errors.New("codegen: Scaffold targets have exported methods no Resolver asks for")

// Generate writes the bindings cfg describes under Config.Output, deletes the
// files an earlier generate wrote that this one does not, and scaffolds the
// Resolver methods Config.Scaffold's targets lack. Every refusal is found
// before anything is written, so a refused generate leaves the tree as it was.
// With Config.Verify it then builds the output.
//
// The context is honoured until the first write: loading packages for
// AutoBind, which runs the go command, is where a generate spends its time.
// Once writing starts it finishes, since a write abandoned halfway leaves a
// tree that is neither this run's nor the last.
func Generate(ctx context.Context, cfg Config) error {
	r, err := plan(ctx, cfg)
	if err != nil {
		return err
	}
	return r.apply(ctx)
}

// CheckOptions selects what Check looks for beyond stale output.
type CheckOptions struct {
	// Stubs also fails, with an error wrapping ErrStubs, while a Scaffold
	// target has a method whose body is still the panic scaffolding wrote.
	// Such a stub compiles and passes every test that does not select its
	// field, so the compiler cannot report it. It needs Config.Scaffold, which
	// says where the implementations are.
	Stubs bool
	// Orphans also fails, with an error wrapping ErrOrphans, while a Scaffold
	// target has an exported method that none of the Resolvers it implements
	// has. A field dropped from the SDL leaves its method behind, and the
	// method compiles, so the compiler cannot report it; a generate says so in
	// a note, which a CI job does not see. It is opt-in because an exported
	// method can be the author's own, such as String, and the check cannot
	// tell: asking for it says every exported method of the target is a
	// resolver. It needs Config.Scaffold, which says where the implementations
	// are.
	Orphans bool
}

// Check is a generate that writes and deletes nothing. It returns an error
// wrapping ErrStale that names each file a generate would create, rewrite or
// delete, relative to Config.Dir, or nil when the output is current. It
// refuses whatever a generate would refuse, so a configuration error is never
// reported as up to date.
//
// A field a generate would answer with something else -- a Resolver method, a
// field or method of the bound type -- is named below the files, since the
// value served changes with it.
//
// Missing scaffolded methods are not checked, beyond refusing a scaffold
// configuration a generate would refuse: scaffolding adds to code the author
// owns, so whether it is all there is the compiler's to say. A stub left
// unimplemented compiles, which is what CheckOptions.Stubs is for. A file that
// differs only in line endings is unchanged, here as for a generate.
func Check(ctx context.Context, cfg Config, opts CheckOptions) error {
	if opts.Stubs && len(cfg.Scaffold) == 0 {
		return errors.New("codegen: CheckOptions.Stubs looks for stubs in the Scaffold targets, and Scaffold names none")
	}
	if opts.Orphans && len(cfg.Scaffold) == 0 {
		return errors.New("codegen: CheckOptions.Orphans looks for orphan methods in the Scaffold targets, and Scaffold names none")
	}
	r, err := plan(ctx, cfg)
	if err != nil {
		return err
	}
	err = r.b.check(r.outDir, r.rels, r.olds, r.files, r.recorded, r.changes)
	if opts.Stubs {
		err = errors.Join(err, r.b.checkStubs())
	}
	if opts.Orphans {
		err = errors.Join(err, r.b.checkOrphans())
	}
	return err
}

// Render returns the files a generate would write, keyed by slash-separated
// path relative to Config.Output, and writes, deletes and scaffolds nothing. It
// refuses whatever is wrong with the schema or the configuration, as a
// generate does; what only a write would find -- a file under Output that is
// not gqlc's, a scaffold target that clashes with generated code -- is not
// looked for, since nothing is written. It reads the schema.go an earlier
// generate left, as a generate does, because which custom scalars have
// already been reported is recorded there.
//
// It is for a tool that wants the output without owning a directory: an
// editor previewing a change, a test asserting on generated text.
func Render(ctx context.Context, cfg Config) (map[string][]byte, error) {
	_, _, files, err := render(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(files))
	for rel, src := range files {
		out[filepath.ToSlash(rel)] = src
	}
	return out, nil
}

// run is a generate that every refusal has passed: what it would write, and
// what is on disk where it would write it.
type run struct {
	b        *builder
	dir      string
	outDir   string
	files    map[string][]byte
	rels     []string
	olds     [][]byte
	recorded []string
	changes  []string
}

// render is the half of a generate that reads the schema and the
// configuration and produces the files: every refusal about either is made
// here. It returns the builder, the copies the last generate recorded, and the
// files keyed by their path under Output.
func render(ctx context.Context, cfg Config) (*builder, []string, map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if len(cfg.SchemaGlobs) == 0 {
		return nil, nil, nil, errors.New("codegen: SchemaGlobs is empty")
	}
	if cfg.Output == "" {
		return nil, nil, nil, errors.New("codegen: Output is empty")
	}
	if cfg.Package == "" {
		return nil, nil, nil, errors.New("codegen: Package is empty")
	}
	if err := checkGroupDir(cfg.GroupDir); err != nil {
		return nil, nil, nil, err
	}
	if err := checkGoConfig(cfg); err != nil {
		return nil, nil, nil, err
	}
	dir := cfg.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	cfg.Dir = dir
	outDir := outputDir(dir, cfg.Output)
	// Before the schema is loaded: a refused GroupDir costs nothing to report.
	if err := checkGroupDirRoot(outDir, cfg.GroupDir); err != nil {
		return nil, nil, nil, err
	}
	b, err := newBuilder(ctx, dir, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := b.checkInline(); err != nil {
		return nil, nil, nil, err
	}
	if err := b.expandScaffold(b.uniqueGroups()); err != nil {
		return nil, nil, nil, err
	}
	// What the last generate left for this one. Read for what it says and
	// nothing else: whether the file is gqlc's to replace is asked later, of
	// every file.
	var recorded []string
	if last, err := os.ReadFile(filepath.Join(outDir, "schema.go")); err == nil {
		recorded, b.scalarsNamedBefore = recordedIn(last)
	}
	recorded = b.trustedCopies(recorded)
	files, err := b.emit()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	// A scaffold key that names no group is a configuration error, refused
	// here so that Render refuses it too.
	if err := b.checkScaffold(b.uniqueGroups()); err != nil {
		return nil, nil, nil, err
	}
	return b, recorded, files, nil
}

// plan is render and then everything a write would find on disk, in the
// order a generate has always found them: every refusal before any write.
func plan(ctx context.Context, cfg Config) (*run, error) {
	b, recorded, files, err := render(ctx, cfg)
	if err != nil {
		return nil, err
	}
	outDir := outputDir(b.dir, b.cfg.Output)
	if err := b.checkGroupRoots(outDir); err != nil {
		return nil, err
	}
	// Read concurrently: each write first reads the old file to leave an
	// unchanged one alone, and with a package per group that is thousands of
	// opens, which on Windows was most of a generate.
	rels := slices.Sorted(maps.Keys(files))
	errs := make([]error, len(rels))
	olds := make([][]byte, len(rels))
	boundedEach(len(rels), func(i int) {
		olds[i], errs[i] = readExisting(filepath.Join(outDir, rels[i]))
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := b.foreignSchemaFiles(outDir, files, recorded, rels, olds); err != nil {
		return nil, err
	}
	// What scaffolding would refuse on disk came last once, after the writes:
	// a target clashing with generated code left a tree half this run's, and
	// a check, which stops before writing, called it current.
	if err := b.checkScaffoldTargets(outDir, files); err != nil {
		return nil, err
	}
	// The last point a cancellation can stop a generate: everything after is
	// apply, and a write abandoned halfway leaves a tree that is no run's.
	// Between render's check and this one a large schema reads thousands of
	// output files and parses every scaffold target's package.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &run{
		b: b, dir: b.dir, outDir: outDir, files: files, rels: rels, olds: olds,
		recorded: recorded, changes: b.bindingChanges(rels, olds, files),
	}, nil
}

// apply writes what plan found, prunes what the last generate wrote and this
// one does not, scaffolds, and verifies when asked.
func (r *run) apply(ctx context.Context) error {
	b := r.b
	b.reportBindingChanges(r.changes)
	errs := make([]error, len(r.rels))
	boundedEach(len(r.rels), func(i int) {
		errs[i] = writeGo(filepath.Join(r.outDir, r.rels[i]), r.olds[i], r.files[r.rels[i]])
	})
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := pruneGenerated(r.outDir, r.files, r.recorded, b.notef); err != nil {
		return err
	}
	if err := b.scaffold(b.uniqueGroups()); err != nil {
		return err
	}
	if b.cfg.Verify {
		return verifyBuild(ctx, r.dir, r.outDir)
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
	// The whole path, not only its last element: it is written into every
	// generated import, and a go.mod line read with its trailing comment made
	// it "example.com/app // the app/graph", which passed the check above.
	if cfg.Package != "" && !importPathOK(cfg.Package) {
		return fmt.Errorf("codegen: Package %q is not an import path", cfg.Package)
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
