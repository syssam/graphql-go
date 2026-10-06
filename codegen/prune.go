package codegen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// writtenSet is the files a run wrote, relative to Output.
type writtenSet struct {
	outDir string
	exact  map[string]bool
	folded map[string][]string // lower-cased path -> each spelling written
}

func writtenKeys(outDir string, written map[string][]byte) writtenSet {
	s := writtenSet{outDir: outDir, exact: make(map[string]bool, len(written)), folded: make(map[string][]string, len(written))}
	for rel := range written {
		rel = filepath.ToSlash(rel)
		s.exact[rel] = true
		s.folded[strings.ToLower(rel)] = append(s.folded[strings.ToLower(rel)], rel)
	}
	return s
}

// has reports whether rel is a file this run wrote: the same path, or the
// same file reached under a spelling that differs in case. On Windows and
// macOS a file written as register/user/generated.go into an existing
// Register/ is walked under that spelling, and an exact compare deleted what
// the run had just written. Folding case in the compare itself was wrong the
// other way: on Linux schema/User.graphql and schema/user.graphql are two
// files, and keeping the stale one embeds both. Only the filesystem knows, so
// it is asked.
func (s writtenSet) has(rel string) bool {
	rel = filepath.ToSlash(rel)
	if s.exact[rel] {
		return true
	}
	for _, w := range s.folded[strings.ToLower(rel)] {
		if samePath(filepath.Join(s.outDir, rel), filepath.Join(s.outDir, w)) {
			return true
		}
	}
	return false
}

// pruneGenerated deletes what gqlc owns under outDir and this run did not
// write, then any directory that deleting it left empty. It owns two kinds of
// file. Go files carrying generatedHeader: a group whose types all moved to
// Go bindings of their own stopped getting a models.go, and the old one
// stayed, compiling, so nothing said it was dead. And the SDL copies in
// outDir/schema, which the root package embeds by glob: a copy whose source
// was deleted or renamed stayed embedded and its types kept answering. A copy
// carries no header, so which files there are gqlc's is what the last run's
// schema.go lists, given here as recorded.
//
// Files without the header -- hand-written ones, scaffolded resolvers,
// another generator's output -- are never touched, and neither is an SDL file
// the list does not name: Generate has refused that one by now. Neither is a
// directory the go command itself ignores (testdata, vendor, and names
// starting with "." or "_"), where a golden copy of gqlc output is a fixture,
// not a stale file; nor another gqlc run's output nested in this one's.
//
// A Go file it cannot delete -- held open by an editor, read-only -- is named
// through notef and left: it is dead code that still compiles, and failing the
// generate there left scaffolding undone and every later run failing. An SDL
// copy it cannot delete fails the generate: the root embeds every copy, so the
// types it holds would go on being served, and a library caller with no Notef
// would never hear of it.
func pruneGenerated(outDir string, written map[string][]byte, recorded []string, notef func(string, ...any)) error {
	root, ours, stale, err := pruneList(outDir, written, recorded)
	if err != nil {
		return err
	}
	for _, f := range removeStale(root, ours) {
		notef("could not delete %s, which this run no longer generates: %v", f.path, f.err)
	}
	var errs []error
	for _, f := range removeStale(root, stale) {
		errs = append(errs, fmt.Errorf("codegen: %s is no longer generated but is still embedded, and could not be deleted: %w", f.path, f.err))
	}
	return errors.Join(errs...)
}

// pruneList is what pruneGenerated deletes, without deleting it: the
// generated Go files under root that this run did not write, and the stale
// SDL copies. root is Output's real path, which the first are relative to.
func pruneList(outDir string, written map[string][]byte, recorded []string) (root string, ours, stale []string, err error) {
	keys := writtenKeys(outDir, written)
	// The SDL copies by glob, which follows a linked Output or schema/ the way
	// the embed reading them does; a walk does not.
	if stale, _, err = schemaLeftovers(outDir, keys, recorded); err != nil {
		return "", nil, nil, err
	}
	// Walked from Output's real path, for the same reason: WalkDir does not
	// follow a link at its root, and a linked Output was pruned of nothing.
	root = outDir
	if real, err := filepath.EvalSymlinks(outDir); err == nil {
		root = real
	}
	var candidates []string
	if err := filepath.WalkDir(root, pruneWalker(root, keys, &candidates)); err != nil {
		return "", nil, nil, err
	}
	ours, err = generatedAmong(candidates)
	return root, ours, stale, err
}

// removeFailure is a stale path removeStale could not delete, and why.
type removeFailure struct {
	path string
	err  error
}

// removeStale deletes each path, then each directory that leaves empty up to
// root, and returns what it could not delete; one failure does not stop the
// rest. The caller decides what a failure means.
func removeStale(root string, stale []string) []removeFailure {
	var failed []removeFailure
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failed = append(failed, removeFailure{path, err})
			}
			continue
		}
		// os.Remove refuses a directory that is not empty, which is the check;
		// but it removes a link to one whatever is behind it, and the link is
		// the author's.
		for dir := filepath.Dir(path); dir != root; dir = filepath.Dir(dir) {
			if info, err := os.Lstat(dir); err != nil || info.Mode()&fs.ModeSymlink != 0 || os.Remove(dir) != nil {
				break
			}
		}
	}
	return failed
}

// pruneWalker is pruneGenerated's walk: it decides which directories to
// enter, and appends to candidates each Go file this run did not write, whose
// header decides.
func pruneWalker(outDir string, keys writtenSet, candidates *[]string) fs.WalkDirFunc {
	return func(path string, d fs.DirEntry, err error) error {
		// Something removed while walking -- another generate, an editor's
		// temporary directory -- is skipped, not the end of the walk: stopping
		// there left every stale file after it in place and reported success.
		// So is what this process may not read (skippable).
		if skippable(err) {
			if d != nil && d.IsDir() && path != outDir {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == outDir {
				return nil
			}
			if ignoredDir(d.Name()) {
				return filepath.SkipDir
			}
			// Another module's tree -- what an Output of "." walks into -- is
			// that module's, as the go command treats it.
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			nested, err := isGqlcRoot(path)
			switch {
			case skippable(err), nested:
				return filepath.SkipDir
			case err != nil:
				return err
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		if !keys.has(rel) {
			*candidates = append(*candidates, path)
		}
		return nil
	}
}

// generatedAmong is the paths whose file opens with gqlc's header, read
// sixteen at a time as the writes are: in a layout with a hand-written
// resolver beside every group, each is opened here, and serially that was
// most of the prune. A file gone before it is read is skipped.
func generatedAmong(paths []string) ([]string, error) {
	ours := make([]bool, len(paths))
	errs := make([]error, len(paths))
	boundedEach(len(paths), func(i int) {
		ok, err := hasGeneratedHeader(paths[i])
		if skippable(err) {
			return
		}
		ours[i], errs[i] = ok, err
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var out []string
	for i, p := range paths {
		if ours[i] {
			out = append(out, p)
		}
	}
	return out, nil
}

// skippable is an error pruning steps around rather than fails on: an entry
// gone since it was listed, or one this process may not read. Pruning is
// cleanup after the output is written; an unreadable directory elsewhere in
// Output holds nothing it could delete, and failing the whole generate there
// left scaffolding undone and every later run failing the same way.
func skippable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// isGqlcRoot reports whether dir is the output root of a gqlc run: its own
// embedded schema/ directory beside a gqlc-generated schema.go. Either alone
// is not enough -- a group package can hold a hand-written schema/ of its
// own, and skipping it would leave that group's stale files forever.
func isGqlcRoot(dir string) (bool, error) {
	fi, err := os.Stat(filepath.Join(dir, "schema"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	ours, err := hasGeneratedHeader(filepath.Join(dir, "schema.go"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return ours, err
}
