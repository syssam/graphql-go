package codegen

import (
	"context"
	"fmt"
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
		return fmt.Errorf("codegen: SchemaGlobs is empty")
	}
	if cfg.Output == "" {
		return fmt.Errorf("codegen: Output is empty")
	}
	if cfg.Package == "" {
		return fmt.Errorf("codegen: Package is empty")
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
