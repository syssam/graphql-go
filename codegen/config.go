package codegen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Config drives Generate. A Manifest is not required: SDL types without a
// Models or Manifest entry get a generated model in Output/model.
type Config struct {
	// Dir is the working directory for SchemaGlobs and Output. Empty means
	// the process working directory.
	Dir string
	// SchemaGlobs lists SDL files relative to Dir.
	SchemaGlobs []string
	// Output is the directory for generated Go, relative to Dir.
	Output string
	// Package is the import path of Output. Required so generated model
	// imports are correct.
	Package string
	// GroupFunc assigns a GraphQL type to a generated package. The
	// second argument is the SDL file that defined the type. A nil
	// function groups by the SDL file stem. Names schema, model and the
	// output package are remapped so they do not collide with embed or
	// model directories. One group stays flat in Output; two or more
	// become subpackages plus a Resolvers struct.
	GroupFunc func(typeName, sdlFile string) string
	// Models maps a GraphQL named type to a Go type expression
	// (for example Time → time.Time). Unmapped custom scalars become
	// named string types in the model package.
	Models map[string]string
	// ModelDirective names an SDL directive that already carries a type's Go
	// binding, and the argument holding it. Empty means none.
	//
	// A schema arriving from another generator has those bindings written down
	// already -- gqlgen spells it @goModel(model: "pkg/path.Type") -- and one
	// real schema carries 3 778 of them. Restating those in Models is work with
	// no decision in it, and a translation step that has to be rerun whenever
	// the SDL changes is a second source of truth.
	//
	// The spelling is declared rather than hardcoded, for the reason
	// RequirementDirective gives in the root package: a generator that only
	// understands one vendor's directive name is a generator that has to be
	// forked to understand the next one. Models still wins where both name a
	// type, so a config can override what the SDL says.
	ModelDirective ModelDirective
	// Manifest binds GraphQL types and fields explicitly instead of letting
	// the generator infer them. It loads no Go type information; see the
	// Manifest documentation.
	Manifest *Manifest
	// AutoBind names package patterns to discover bindings from. Only those
	// packages are loaded, and only their export data: no syntax trees and no
	// function bodies. A Manifest entry overrides discovery field by field.
	AutoBind []string
	// NullableInputOmittable uses graphql.Omittable[*T] for nullable
	// input-object fields so PATCH-style inputs distinguish absent from
	// null. Field arguments stay pointers.
	NullableInputOmittable bool
}

// Generate writes bindings, models, argument structs and a Resolver
// interface from SDL. It never loads Go packages.
func Generate(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cfg.SchemaGlobs) == 0 {
		return errors.New("codegen: SchemaGlobs is empty")
	}
	if cfg.Output == "" {
		return errors.New("codegen: Output is empty")
	}
	if cfg.Package == "" {
		return errors.New("codegen: Package is empty")
	}
	dir := cfg.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	b, err := newBuilder(dir, cfg)
	if err != nil {
		return err
	}
	files, err := b.emit()
	if err != nil {
		return err
	}
	outDir := filepath.Join(dir, cfg.Output)
	for rel, src := range files {
		if err := writeGo(filepath.Join(outDir, rel), src); err != nil {
			return err
		}
	}
	return nil
}

// ModelDirective identifies an SDL directive carrying a Go type binding.
type ModelDirective struct {
	// Name is the directive, without the @.
	Name string
	// Arg is the argument holding the Go type expression.
	Arg string
}

// IsZero reports that no directive was named.
func (d ModelDirective) IsZero() bool { return d.Name == "" || d.Arg == "" }
