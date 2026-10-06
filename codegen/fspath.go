package codegen

import (
	"os"
	"path/filepath"
	"strings"
)

// realPath is p with links resolved, as far up as p exists: a file a
// generate would create is in a directory that may not exist yet, and its
// nearest existing ancestor decides where it is.
func realPath(p string) string {
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(dir) == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// samePath reports whether a and b are one file or directory: by path, or --
// when both exist -- by identity, so a spelling that differs in letter case
// on a case-insensitive filesystem, or a path through a link, is still the
// same one, and on a case-sensitive filesystem two spellings stay two.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// onDisk is dir, under base, with each element as the filesystem spells it,
// or "" when it does not exist. On a filesystem that ignores case a path
// opens whatever case it is written in, and only the directory listing says
// what the name is.
func onDisk(base, dir string) string {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if _, err := os.Stat(dir); err != nil {
			return ""
		}
		return dir
	}
	at := base
	if rel == "." {
		return at
	}
	for _, elem := range strings.Split(rel, string(filepath.Separator)) {
		entries, err := os.ReadDir(at)
		if err != nil {
			return ""
		}
		name := ""
		for _, e := range entries {
			if e.Name() == elem {
				name = elem
				break
			}
			if name == "" && strings.EqualFold(e.Name(), elem) {
				name = e.Name()
			}
		}
		if name == "" {
			return ""
		}
		at = filepath.Join(at, name)
	}
	if !samePath(at, dir) {
		return ""
	}
	return at
}
