package codegen

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// sourceFilesMatching is the files a schema glob names, sorted. It is
// filepath.Glob with two differences. A path element that is exactly ** stands
// for any number of directories, none included, so schema/**/*.graphql is
// every SDL file under schema/ at any depth: filepath.Glob reads ** as *, one
// level and no other, which left out the files beside the pattern and the
// ones two levels down, silently. And a directory a pattern matches is not a
// source, where reading it as one failed the generate.
//
// ** does not descend into a directory whose name starts with a dot, as a
// shell's does not: **/*.graphql from a project's root would otherwise read
// .git and every editor's cache. Nor into copies, the directory gqlc writes
// its copies of the sources to, which the same pattern would otherwise find
// on the second run and read as a second source of every type. A pattern
// that names either directory outright still reads it.
func sourceFilesMatching(pattern, copies string) ([]string, error) {
	seen := map[string]bool{}
	if err := matchSources(filepath.Clean(pattern), copies, seen); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

func matchSources(pattern, copies string, into map[string]bool) error {
	elems := strings.Split(filepath.ToSlash(pattern), "/")
	star := slices.Index(elems, "**")
	if star < 0 {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
				into[m] = true
			}
		}
		return nil
	}
	base := filepath.FromSlash(strings.Join(elems[:star], "/"))
	switch {
	case star == 0:
		base = "."
	case base == "":
		base = string(filepath.Separator)
	}
	rest := filepath.FromSlash(strings.Join(elems[star+1:], "/"))
	if rest == "" {
		// Nothing after it: every file beneath.
		rest = "*"
	}
	roots, err := filepath.Glob(base)
	if err != nil {
		return err
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			if path != root && (strings.HasPrefix(d.Name(), ".") || (strings.EqualFold(d.Name(), filepath.Base(copies)) && samePath(path, copies))) {
				return filepath.SkipDir
			}
			return matchSources(filepath.Join(globLiteral(path), rest), copies, into)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// globLiteral is path as a pattern that matches only itself. The directories
// ** walks into are names on disk and may hold a pattern's characters:
// routes/[id] read as a pattern is a character class its own name does not
// match. Each is written as a class of one, which means the same on every
// platform; a backslash escape does not, being the separator on Windows. A
// backslash in a name, where it is not the separator, is doubled.
func globLiteral(path string) string {
	if !strings.ContainsAny(path, `*?[\`) {
		return path
	}
	var b strings.Builder
	for _, r := range path {
		switch {
		case r == '*' || r == '?' || r == '[':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
		case r == '\\' && filepath.Separator != '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
