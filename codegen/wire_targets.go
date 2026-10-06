package codegen

import (
	"fmt"
	"path/filepath"
	"strings"
)

// wireDir is a target directory under the one spelling wire.go may use for
// it. Go will not import two paths that differ only in letter case into one
// package, so a directory met under two spellings -- two scaffold targets, or
// a target and a package generated code imports anyway -- is one import where
// the filesystem says both are one directory, and an error where it says they
// are not, or cannot say because nothing exists yet and a guess would compile
// on one machine and not on another. The spelling is the directory's name on
// disk where it has one, since that is what every other file importing the
// package uses; the first one met in the configuration may be the wrong one.
func (b *builder) wireDir(dir string) (string, error) {
	fold := strings.ToLower(dir)
	first, seen := b.wire.dirs[fold]
	if seen && first == dir {
		return dir, nil
	}
	real := onDisk(b.dir, dir)
	switch {
	case !seen:
		if real != "" {
			dir = real
		}
		b.wire.dirs[fold] = dir
		return dir, nil
	case real != "" && samePath(first, dir):
		return first, nil
	}
	rel := func(d string) string {
		if r, err := filepath.Rel(b.dir, d); err == nil {
			return filepath.ToSlash(r)
		}
		return d
	}
	return "", fmt.Errorf("codegen: wire: %s and %s differ only in letter case, and Go will not import both into one package; spell the directory one way", rel(first), rel(dir))
}

// targetKey is what makes two scaffold targets one implementation: the
// directory, under the spelling wire.go uses for it, and the type.
func (b *builder) targetKey(target string) string {
	dir, typ, err := b.splitTarget(target)
	if err != nil {
		return target
	}
	if d, err := b.wireDir(dir); err == nil {
		dir = d
	}
	return dir + "\x00" + typ
}

// wireShared reports whether target implements more than one group.
func (b *builder) wireShared(target string) bool {
	b.readWire()
	dir, typ, err := b.splitTarget(target)
	if err != nil {
		return false
	}
	if dir, err = b.wireDir(dir); err != nil {
		return false
	}
	return b.wire.targets[dir+"\x00"+typ] > 1
}
