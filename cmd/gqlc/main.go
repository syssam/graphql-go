// Command gqlc generates GraphQL bindings from SDL, driven by a gqlc.yaml.
//
//	gqlc -config gqlc.yaml
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/syssam/graphql-go/codegen"
	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	Schema                 []string `yaml:"schema"`
	Output                 string   `yaml:"output"`
	Package                string   `yaml:"package"`
	NullableInputOmittable bool     `yaml:"nullableInputOmittable"`
	// zeroForNullInputs lets a Go field that cannot be null back a nullable
	// input position; read the graphql.ZeroForNull godoc before setting it.
	ZeroForNullInputs bool              `yaml:"zeroForNullInputs"`
	Models            map[string]string `yaml:"models"`
	// modelDirective reads type bindings the SDL already carries, for a schema
	// arriving from another generator:
	//
	//	modelDirective:
	//	  name: goModel
	//	  arg: model
	ModelDirective struct {
		Name string `yaml:"name"`
		Arg  string `yaml:"arg"`
	} `yaml:"modelDirective"`
	// fieldDirective forces a field to the Resolver interface, for the same
	// reason: gqlgen spells it @goField(forceResolver: true).
	//
	//	fieldDirective:
	//	  name: goField
	//	  forceResolverArg: forceResolver
	FieldDirective struct {
		Name             string `yaml:"name"`
		ForceResolverArg string `yaml:"forceResolverArg"`
	} `yaml:"fieldDirective"`
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
	// KnownFields, because a misspelled key is otherwise ignored and the
	// generator quietly does the default thing: nullableInputOmitable with one
	// t produces models with no Omittable and no hint why.
	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("gqlc: parse %s: %w", *configPath, err)
	}
	cfg := codegen.Config{
		Dir:                    filepath.Dir(*configPath),
		SchemaGlobs:            fc.Schema,
		Output:                 fc.Output,
		Package:                fc.Package,
		Models:                 fc.Models,
		NullableInputOmittable: fc.NullableInputOmittable,
		ZeroForNullInputs:      fc.ZeroForNullInputs,
		ModelDirective:         codegen.ModelDirective{Name: fc.ModelDirective.Name, Arg: fc.ModelDirective.Arg},
		FieldDirective:         codegen.FieldDirective{Name: fc.FieldDirective.Name, ForceResolverArg: fc.FieldDirective.ForceResolverArg},
		Notef: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "gqlc: "+format+"\n", args...)
		},
	}
	return codegen.Generate(context.Background(), cfg)
}
