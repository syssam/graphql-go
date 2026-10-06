package codegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkGroupDir refuses a GroupDir that would leave Output, land group
// packages where the model packages or the embedded SDL already are, or name
// a directory the go command ignores: there pruning is skipped, so a
// package left behind would never be deleted, and vendor and dot-names are
// not importable as written.
func checkGroupDir(dir string) error {
	if dir == "" {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(dir))
	// Compared without case: on Windows and macOS "Model" is the directory
	// the models are in, and the collision surfaces as an import cycle that
	// names nothing.
	lower := strings.ToLower(clean)
	switch {
	case filepath.IsAbs(dir) || filepath.VolumeName(dir) != "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/"):
		// VolumeName catches Windows' drive-relative "C:foo", which is not
		// absolute and would be joined into both a path and an import path.
		return fmt.Errorf("codegen: GroupDir %q must be a directory inside Output", dir)
	case !importPathElems(clean):
		// On Linux "C:foo" is a directory like any other, but the colon is no
		// more allowed in the import path it becomes than it is on Windows.
		return fmt.Errorf("codegen: GroupDir %q is not a portable import path: use letters, digits and . _ -, no trailing dot and no reserved device name", dir)
	case lower == "model" || strings.HasPrefix(lower, "model/") || lower == "schema" || strings.HasPrefix(lower, "schema/"):
		return fmt.Errorf("codegen: GroupDir %q is where the generated models or the embedded SDL go", dir)
	}
	for elem := range strings.SplitSeq(clean, "/") {
		if ignoredDir(elem) {
			return fmt.Errorf("codegen: GroupDir %q names %q, a directory the go command ignores", dir, elem)
		}
		// Only code under Output could import a group package beneath an
		// internal/, and the root the application calls is not the one that
		// registers it: the build fails with "use of internal package".
		if strings.EqualFold(elem, "internal") {
			return fmt.Errorf("codegen: GroupDir %q names %q: nothing outside Output could import the group packages", dir, elem)
		}
	}
	return nil
}

// importPathElems reports whether every element of a slash path is one an
// import path may carry and every filesystem can hold as spelled: a
// conservative subset of what the go command accepts. A trailing dot is
// dropped by Windows ("reg." is created as "reg" while the import keeps the
// dot), a reserved device name (con, nul, com1, ...) is not a directory there
// at all, and "~" is how Windows spells a short name.
func importPathElems(p string) bool {
	for elem := range strings.SplitSeq(p, "/") {
		if elem == "" || strings.HasSuffix(elem, ".") || windowsReserved(elem) {
			return false
		}
		for _, r := range elem {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)
			if !ok {
				return false
			}
		}
	}
	return true
}

// windowsReserved reports whether elem, before any extension, is a device
// name Windows reserves in every directory.
func windowsReserved(elem string) bool {
	base, _, _ := strings.Cut(strings.ToLower(elem), ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

// ignoredDir reports whether the go command skips a directory of this name.
func ignoredDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// checkGroupDirRoot refuses a GroupDir that reaches into another gqlc run's
// output root: that run would prune the group packages this one writes
// there, and this one skips pruning them, so each regenerate undoes the other.
func checkGroupDirRoot(outDir, groupDir string) error {
	if groupDir == "" {
		return nil
	}
	dir := outDir
	for elem := range strings.SplitSeq(filepath.ToSlash(filepath.Clean(groupDir)), "/") {
		dir = filepath.Join(dir, elem)
		if err := foreignDir(dir); err != nil {
			return fmt.Errorf("codegen: GroupDir %q: %w", groupDir, err)
		}
	}
	return nil
}

// foreignDir refuses a directory this run must not write into: another gqlc
// run's output root, where that run prunes what this one writes, or another
// module's, where an import path under Package names the other module. A
// path that does not exist yet is this run's to create.
func foreignDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is a file, not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return fmt.Errorf("%s is another module's (it has its own go.mod)", dir)
	}
	root, err := isGqlcRoot(dir)
	if err != nil {
		return err
	}
	if root {
		return fmt.Errorf("%s is another gqlc run's output -- another config's, or an earlier layout of this one, which is yours to move or delete", dir)
	}
	return nil
}

// checkGroupRoots refuses a group package directory that is itself another
// gqlc run's root or module: the two runs would overwrite and prune each
// other's files on every generate, with nothing naming the collision.
//
// The prune walk that follows probes these directories again. Skipping that
// for the ones checked here measured 940 ms against 969 ms at 300 entities,
// p=0.071 over 12 interleaved runs: not distinguishable, so not kept.
func (b *builder) checkGroupRoots(outDir string) error {
	groups := b.uniqueGroups()
	if len(groups) <= 1 {
		return nil
	}
	errs := make([]error, len(groups))
	boundedEach(len(groups), func(i int) {
		if err := foreignDir(filepath.Join(outDir, filepath.FromSlash(b.groupRel(groups[i])))); err != nil {
			errs[i] = fmt.Errorf("codegen: group %s: %w", groups[i], err)
		}
	})
	return errors.Join(errs...)
}
