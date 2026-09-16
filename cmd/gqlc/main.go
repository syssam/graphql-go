// Command gqlc generates GraphQL bindings from SDL, driven by a gqlc.yaml.
//
//	gqlc -config gqlc.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/syssam/graphql-go/codegen"
	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	Schema                 []string          `yaml:"schema"`
	Output                 string            `yaml:"output"`
	Package                string            `yaml:"package"`
	NullableInputOmittable bool              `yaml:"nullableInputOmittable"`
	Models                 map[string]string `yaml:"models"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gqlc", flag.ContinueOnError)
	configPath := fs.String("config", "gqlc.yaml", "path to gqlc.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("gqlc: read %s: %w", *configPath, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return fmt.Errorf("gqlc: parse %s: %w", *configPath, err)
	}
	cfg := codegen.Config{
		Dir:                    filepath.Dir(*configPath),
		SchemaGlobs:            fc.Schema,
		Output:                 fc.Output,
		Package:                fc.Package,
		Models:                 fc.Models,
		NullableInputOmittable: fc.NullableInputOmittable,
	}
	return codegen.Generate(context.Background(), cfg)
}
