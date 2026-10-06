package graphql

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The root package is what every consumer links, so what it imports is what
// every consumer's build inherits: gqlparser/v2 and the standard library, and
// nothing else. The compiler cannot hold that line. go.mod already requires
// yaml, a websocket library, Echo, Fiber and OpenTelemetry for the
// sub-packages that use them, so importing any of them here builds without a
// go.mod change for a reviewer to notice.
//
// The module's own packages are followed, because an internal package the
// root imports is linked with it: a dependency added there is added here.
func TestRootPackageImportsOnlyGqlparserAndTheStandardLibrary(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	const gqlparser = "github.com/vektah/gqlparser/v2"
	module := modulePath(t, root)

	fset := token.NewFileSet()
	seen := map[string]bool{root: true}
	sawGqlparser := false
	for queue := []string{root}; len(queue) > 0; queue = queue[1:] {
		dir := queue[0]
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			file := filepath.Join(dir, e.Name())
			f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				first, _, _ := strings.Cut(path, "/")
				switch rel, own := strings.CutPrefix(path, module+"/"); {
				case own:
					if next := filepath.Join(root, filepath.FromSlash(rel)); !seen[next] {
						seen[next] = true
						queue = append(queue, next)
					}
				case path == gqlparser || strings.HasPrefix(path, gqlparser+"/"):
					sawGqlparser = true
				// A standard library path has no domain in front of it.
				case !strings.Contains(first, "."):
				default:
					name, _ := filepath.Rel(root, file)
					t.Errorf("%s imports %s: the root package and what it links may import only gqlparser/v2 and the standard library", filepath.ToSlash(name), path)
				}
			}
		}
	}
	// A walk that read nothing would agree with any rule.
	if !sawGqlparser {
		t.Fatalf("no import of %s was found from %s, so this did not read the package", gqlparser, root)
	}
}
