package buildbench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const gqlcConfig = `schema:
  - %s
output: graph
package: buildbench/graph
nullableInputOmittable: true
models:
  Time: time.Time
`

const gqlgenConfig = `schema:
  - %s
exec:
  filename: graph/generated.go
  package: graph
model:
  filename: graph/model/models_gen.go
  package: model
resolver:
  layout: follow-schema
  dir: graph
  package: graph
`

// warmFile imports the runtime the generated code will depend on, so that
// building it before the timed steps leaves only the generated packages to
// compile.
var warmFile = map[string]string{
	"gqlc": `package warm

import _ "github.com/syssam/graphql-go"
`,
	"gqlgen": `package warm

import (
	_ "github.com/99designs/gqlgen/graphql"
	_ "github.com/99designs/gqlgen/graphql/introspection"
	_ "github.com/vektah/gqlparser/v2"
	_ "github.com/vektah/gqlparser/v2/ast"
)
`,
}

func writeModule(dir, repoRoot, engine string, n int, split bool) error {
	gomod, err := benchGoMod(repoRoot)
	if err != nil {
		return err
	}
	files := map[string]string{
		"go.mod":       gomod,
		"warm/warm.go": warmFile[engine],
		"gen/main.go":  gqlgenDriver,
	}

	// The schema layout is the variable under test: one file puts every type
	// in a single gqlc group, one file per entity gives a package per entity.
	glob := "schema.graphql"
	if split {
		glob = "schema/*.graphql"
		for name, body := range SDLFiles(n) {
			files["schema/"+name] = body
		}
	} else {
		files["schema.graphql"] = SDL(n)
	}
	switch engine {
	case "gqlc":
		files["gqlc.yaml"] = fmt.Sprintf(gqlcConfig, glob)
	case "gqlgen":
		files["gqlgen.yml"] = fmt.Sprintf(gqlgenConfig, glob)
	default:
		return fmt.Errorf("buildbench: unknown engine %q", engine)
	}

	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}

	// A copied go.sum lets the temporary module build without the network.
	sum, err := os.ReadFile(filepath.Join(repoRoot, "benchmarks", "go.sum"))
	if err != nil {
		return fmt.Errorf("read benchmarks go.sum: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
}

// RepoRoot walks up from the working directory to the module root that holds
// go.mod for github.com/syssam/graphql-go.
func RepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/syssam/graphql-go\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("buildbench: repo root not found from %s", start)
		}
		dir = parent
	}
}

// gqlgenDriver calls gqlgen's codegen directly. The gqlgen CLI main pulls in
// an argument-parsing library that is not otherwise in the module graph, and
// measuring the CLI would add its startup to gqlgen's generation time.
const gqlgenDriver = `package main

import (
	"log"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
)

func main() {
	cfg, err := config.LoadConfig("gqlgen.yml")
	if err != nil {
		log.Fatal(err)
	}
	if err := api.Generate(cfg); err != nil {
		log.Fatal(err)
	}
}
`

// benchGoMod derives the temporary module's go.mod from the benchmarks
// module, so both resolve to exactly the same dependency versions and the
// copied go.sum is valid.
func benchGoMod(repoRoot string) (string, error) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "benchmarks", "go.mod"))
	if err != nil {
		return "", err
	}
	out := strings.Replace(string(b),
		"module github.com/syssam/graphql-go/benchmarks",
		"module buildbench", 1)
	out = strings.Replace(out,
		"replace github.com/syssam/graphql-go => ../",
		"replace github.com/syssam/graphql-go => "+filepath.ToSlash(repoRoot), 1)
	return out, nil
}
