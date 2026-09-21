package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Generated code is code a consumer cannot edit, so a linter complaining that
// its package has no comment is a complaint they cannot act on. Exactly one
// file per generated package carries a package comment: none is a warning for
// every user of gqlc, and more than one gives the package duplicate docs.
func TestGeneratedPackagesHaveExactlyOnePackageComment(t *testing.T) {
	for _, c := range []struct {
		name string
		sdl  string
	}{
		{name: "one group stays flat", sdl: manifestSDL},
		{name: "two groups become subpackages", sdl: manifestSDL + "\ntype Order { id: ID! }\nextend type Query { order(id: ID!): Order }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := generate(t, c.sdl, Config{})
			perPkg := map[string]int{}
			files := 0
			err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
					return err
				}
				files++
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				src := string(b)
				pkg := filepath.Dir(path)
				if _, ok := perPkg[pkg]; !ok {
					perPkg[pkg] = 0
				}
				// A package comment is a comment block directly above the
				// package clause, with no blank line between.
				for _, line := range strings.Split(src, "\n") {
					if strings.HasPrefix(line, "package ") {
						break
					}
					if strings.HasPrefix(line, "// Package ") {
						perPkg[pkg]++
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if files == 0 {
				t.Fatal("no generated files found")
			}
			for pkg, n := range perPkg {
				if n != 1 {
					t.Errorf("%s: %d package comments, want exactly 1", filepath.Base(pkg), n)
				}
			}
		})
	}
}
