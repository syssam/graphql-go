// Command gqlc generates GraphQL bindings from SDL.
//
// For the common case, flags are enough and no config file is needed. The
// output package is derived from the nearest go.mod, so the only two facts it
// cannot work out are the schema and where to put the result:
//
//	gqlc -schema schema.graphql -out graph
//
// Everything else stays in a gqlc.yaml, which is still the way to reach the
// options a flag would make unreadable -- modelDirective and fieldDirective,
// which take a name and an argument each:
//
//	gqlc -config gqlc.yaml
//
// The two forms do not mix: -schema means the flags are the configuration.
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
	var schema, models repeatable
	fs.Var(&schema, "schema", "SDL file or glob; repeat or comma-separate. Implies the flag form.")
	out := fs.String("out", "", "output directory for generated Go")
	pkg := fs.String("pkg", "", "import path of -out; derived from the nearest go.mod when empty")
	fs.Var(&models, "model", "NAME=go/import/path.Type; repeat or comma-separate")
	nullableOmittable := fs.Bool("nullable-input-omittable", false, "give nullable input fields Omittable")
	zeroForNull := fs.Bool("zero-for-null-inputs", false, "let a non-nullable Go field back a nullable input; read the graphql.ZeroForNull godoc first")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if len(schema) > 0 {
		explicitConfig := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				explicitConfig = true
			}
		})
		if explicitConfig {
			return errors.New("gqlc: -config and -schema are two ways to say the same thing; pass one")
		}
		return runFlags(schema, *out, *pkg, models, *nullableOmittable, *zeroForNull)
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

// runFlags is the config-file-free path. Dir stays the process working
// directory, so -schema and -out are read the way a shell would read them.
func runFlags(schema repeatable, out, pkg string, models repeatable, nullableOmittable, zeroForNull bool) error {
	if out == "" {
		return errors.New("gqlc: -out is required with -schema")
	}
	m, err := parseModels(models)
	if err != nil {
		return fmt.Errorf("gqlc: %w", err)
	}
	if pkg == "" {
		if pkg, err = derivePackage(out); err != nil {
			return fmt.Errorf("gqlc: %w", err)
		}
	}
	return codegen.Generate(context.Background(), codegen.Config{
		SchemaGlobs:            schema,
		Output:                 out,
		Package:                pkg,
		Models:                 m,
		NullableInputOmittable: nullableOmittable,
		ZeroForNullInputs:      zeroForNull,
		Notef: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "gqlc: "+format+"\n", args...)
		},
	})
}
