package codegen

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// check is Config.Check: what a generate would create, rewrite and delete,
// named relative to Config.Dir, in that order and sorted within each.
func (b *builder) check(outDir string, rels []string, olds [][]byte, files map[string][]byte, recorded []string) error {
	base, _ := filepath.Abs(b.dir)
	// name puts p, a path on disk, relative to Config.Dir. Both are made
	// absolute first, since either may be given relative to the working
	// directory; and where a symlink lies between them -- Config.Dir given
	// through its real path and Output through a link -- they are compared
	// as the filesystem resolves them.
	under := func(base, p string) (string, bool) {
		r, err := filepath.Rel(base, p)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", false
		}
		return filepath.ToSlash(r), true
	}
	name := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return filepath.ToSlash(p)
		}
		if r, ok := under(base, abs); ok {
			return r
		}
		realBase, err1 := filepath.EvalSymlinks(base)
		if err1 == nil {
			if r, ok := under(realBase, realPath(abs)); ok {
				return r
			}
		}
		return filepath.ToSlash(abs)
	}
	var lines []string
	for _, verb := range []string{"create", "rewrite"} {
		for i, rel := range rels {
			switch {
			case verb == "create" && olds[i] == nil,
				verb == "rewrite" && olds[i] != nil && !sameContent(olds[i], files[rel]):
				lines = append(lines, "  "+verb+" "+name(filepath.Join(outDir, filepath.FromSlash(rel))))
			}
		}
	}
	root, ours, stale, err := pruneList(outDir, files, recorded)
	if err != nil {
		return err
	}
	var gone []string
	for _, p := range ours {
		// Found under Output's real path, and named under Output as it is
		// written, which is the path the author knows it by.
		if r, err := filepath.Rel(root, p); err == nil {
			p = filepath.Join(outDir, r)
		}
		gone = append(gone, name(p))
	}
	// schemaLeftovers globs under outDir, so these are paths under it
	// already.
	for _, p := range stale {
		gone = append(gone, name(p))
	}
	slices.Sort(gone)
	for _, p := range slices.Compact(gone) {
		lines = append(lines, "  delete "+p)
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%w; a generate would\n%s", ErrStale, strings.Join(lines, "\n"))
}
