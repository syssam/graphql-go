package graphql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// docs/api-stability.md names the types still expected to change, and each
// says so in its godoc, which is where a consumer is reading when it decides
// to depend on one. The two are kept in step here: a type added to the list
// without the marker, or marked without being listed, fails.
func TestTypesExpectedToChangeAreMarkedExperimental(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("docs", "api-stability.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(raw), "\n## Still expected to change\n")
	if !found {
		t.Fatal(`docs/api-stability.md has no "Still expected to change" section`)
	}
	section, _, _ = strings.Cut(section, "\n## ")

	// Each bullet opens with the names in bold: - **`A`, `B`.**
	listed := map[string]bool{}
	for _, bullet := range regexp.MustCompile(`(?m)^- \*\*(.+?)\*\*`).FindAllStringSubmatch(section, -1) {
		for _, name := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(bullet[1], -1) {
			listed[name[1]] = true
		}
	}
	if len(listed) < 5 {
		t.Fatalf("read only %v from the section; the test is not reading the list", slices.Sorted(maps.Keys(listed)))
	}

	marked := map[string]bool{}
	for dir, prefix := range map[string]string{".": "", "codegen": "codegen."} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					doc := ts.Doc
					if doc == nil {
						doc = gd.Doc
					}
					if ts.Name.IsExported() && doc != nil && strings.Contains(doc.Text(), "\nExperimental: ") {
						marked[prefix+ts.Name.Name] = true
					}
				}
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(listed)) {
		if !marked[name] {
			t.Errorf("docs/api-stability.md expects %s to change, and its godoc does not say Experimental", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(marked)) {
		if !listed[name] {
			t.Errorf("%s is marked Experimental and docs/api-stability.md does not list it", name)
		}
	}
}
