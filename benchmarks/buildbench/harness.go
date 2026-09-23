package buildbench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Result is one engine's cost for one schema size.
type Result struct {
	Engine     string
	Entities   int
	GenWall    time.Duration
	GenPeakKB  uint64
	GenLOC     int
	GenFiles   int
	GenPkgs    int
	Split      bool
	ColdBuild  time.Duration
	ColdPeakKB uint64
	IncrGen    time.Duration
	Incr       time.Duration
	Err        error
}

// Run measures generation and compilation for one engine at one schema size.
// Each call works in its own temporary module with its own GOCACHE, so a cold
// build is genuinely cold without touching the caller's build cache.
func Run(engine string, n int, repoRoot string, split, keep bool) Result {
	res := Result{Engine: engine, Entities: n, Split: split}

	dir, err := os.MkdirTemp("", fmt.Sprintf("buildbench-%s-%d-", engine, n))
	if err != nil {
		res.Err = err
		return res
	}
	if keep {
		fmt.Fprintf(os.Stderr, "keeping %s\n", dir)
	} else {
		defer func() { _ = os.RemoveAll(dir) }()
	}

	cache := filepath.Join(dir, ".gocache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		res.Err = err
		return res
	}
	env := append(os.Environ(),
		"GOCACHE="+cache,
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
	)

	if err := writeModule(dir, repoRoot, engine, n, split); err != nil {
		res.Err = err
		return res
	}

	// Build the generator, and a stub importing the runtime the generated
	// code will need, before anything is timed. Both populate this case's
	// GOCACHE, so the numbers below measure the schema's cost rather than
	// the cost of compiling the toolchain and the framework once.
	genBin := filepath.Join(dir, "generator"+exeSuffix)
	prep := exec.Command("go", "build", "-o", genBin, generatorPackage(engine))
	prep.Dir = dir
	prep.Env = env
	if _, _, err := timeCmd(prep); err != nil {
		res.Err = fmt.Errorf("build generator: %w", err)
		return res
	}
	warm := exec.Command("go", "build", "./warm")
	warm.Dir = dir
	warm.Env = env
	if _, _, err := timeCmd(warm); err != nil {
		res.Err = fmt.Errorf("warm: %w", err)
		return res
	}

	gen := exec.Command(genBin, generatorArgs(engine)...)
	gen.Dir = dir
	gen.Env = env
	wall, peak, err := timeCmd(gen)
	res.GenWall, res.GenPeakKB = wall, peak
	if err != nil {
		res.Err = fmt.Errorf("generate: %w", err)
		return res
	}

	res.GenLOC, res.GenFiles, res.GenPkgs, err = countGenerated(filepath.Join(dir, "graph"))
	if err != nil {
		res.Err = fmt.Errorf("count: %w", err)
		return res
	}

	// gqlgen type-checks the generated code during generation, which leaves it
	// already compiled in the build cache; gqlc never loads Go packages, so it
	// does not. Comparing builds after that would measure a cache hit against
	// real work. Drop the cache and re-warm the dependencies so both engines
	// face the same starting point: everything but the generated packages.
	if err := resetCache(cache); err != nil {
		res.Err = fmt.Errorf("reset cache: %w", err)
		return res
	}
	rewarm := exec.Command("go", "build", "./warm")
	rewarm.Dir = dir
	rewarm.Env = env
	if _, _, err := timeCmd(rewarm); err != nil {
		res.Err = fmt.Errorf("re-warm: %w", err)
		return res
	}

	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	build.Env = env
	wall, peak, err = timeCmd(build)
	res.ColdBuild, res.ColdPeakKB = wall, peak
	if err != nil {
		res.Err = fmt.Errorf("cold build: %w", err)
		return res
	}

	// Incremental: make a one-entity schema change the way a developer would
	// -- edit that entity's SDL, regenerate, rebuild. Touching a generated
	// file instead would measure the wrong thing: Go keys compilation on a
	// dependency's export data, so a comment edit does not propagate, and the
	// engines keep an entity's code in different places.
	if err := editOneEntity(dir, split); err != nil {
		res.Err = fmt.Errorf("edit entity: %w", err)
		return res
	}
	regen := exec.Command(genBin, generatorArgs(engine)...)
	regen.Dir = dir
	regen.Env = env
	// gqlgen type-checks here, so part of its rebuild cost lands in this step
	// rather than in the build below. Report both and compare the sum.
	res.IncrGen, _, err = timeCmd(regen)
	if err != nil {
		res.Err = fmt.Errorf("regenerate: %w", err)
		return res
	}
	incr := exec.Command("go", "build", "./graph/...")
	incr.Dir = dir
	incr.Env = env
	res.Incr, _, err = timeCmd(incr)
	if err != nil {
		res.Err = fmt.Errorf("incremental build: %w", err)
	}
	return res
}

// editOneEntity adds a field to the first entity, in whichever SDL file holds
// it. This is the "one entity changed" case the per-group split exists for.
func editOneEntity(dir string, split bool) error {
	path := filepath.Join(dir, "schema.graphql")
	if split {
		path = filepath.Join(dir, "schema", strings.ToLower(EntityName(0))+".graphql")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	marker := "type " + EntityName(0) + " implements Node {"
	src := string(b)
	if !strings.Contains(src, marker) {
		return fmt.Errorf("marker %q not found in %s", marker, path)
	}
	src = strings.Replace(src, marker, marker+"\n  addedByBuildbench: String", 1)
	return os.WriteFile(path, []byte(src), 0o644)
}

var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

func generatorPackage(engine string) string {
	switch engine {
	case "gqlc":
		return "github.com/syssam/graphql-go/cmd/gqlc"
	case "gqlgen":
		return "./gen"
	}
	panic("buildbench: unknown engine " + engine)
}

func generatorArgs(engine string) []string {
	switch engine {
	case "gqlc":
		return []string{"-config", "gqlc.yaml"}
	case "gqlgen":
		return nil
	}
	panic("buildbench: unknown engine " + engine)
}

// timeCmd runs c to completion, returning wall time and peak resident memory
// in KB (0 where the platform cannot report it).
func timeCmd(c *exec.Cmd) (time.Duration, uint64, error) {
	var out strings.Builder
	c.Stdout = &out
	c.Stderr = &out

	start := time.Now()
	if err := c.Start(); err != nil {
		return 0, 0, err
	}
	stop := watchPeakRSS(c)
	err := c.Wait()
	wall := time.Since(start)
	peak := stop()
	if err != nil {
		return wall, peak, fmt.Errorf("%w: %s", err, truncate(out.String(), 4000))
	}
	return wall, peak, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

// countGenerated reports generated lines, files and packages. The package
// count is the one that matters for incremental builds: it is how many units
// the Go compiler can rebuild independently.
func countGenerated(root string) (loc, files, pkgs int, err error) {
	dirs := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		dirs[filepath.Dir(path)] = true
		loc += strings.Count(string(b), "\n")
		return nil
	})
	return loc, files, len(dirs), err
}

// resetCache empties the build cache directory without removing it.
func resetCache(cache string) error {
	if err := os.RemoveAll(cache); err != nil {
		return err
	}
	return os.MkdirAll(cache, 0o755)
}
