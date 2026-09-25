package codegen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	// become subpackages, each registered with NewSchema by its Bindings.
	GroupFunc func(typeName, sdlFile string) string
	// RootFieldGroup assigns one field of the query, mutation or subscription
	// root to a group. Empty, or a nil function, means the group GroupFunc
	// (or the file stem) gives the SDL file the field is declared in, so
	// `extend type Query { ... }` in user.graphql lands in the user group.
	//
	// Roots are grouped per field, not per type, because a root is the one
	// type every group adds to: grouped as a type, every root resolver of a
	// large schema ends up in one package, which is the monolith grouping is
	// there to split. A generator that declares every root field in one file
	// (velox does) can follow the returned type instead:
	//
	//	RootFieldGroup: func(f codegen.RootField) string { return f.ReturnGroup }
	RootFieldGroup func(RootField) string
	// Scaffold writes a stub for every Resolver method an implementation does
	// not have yet. It maps a group -- the output package name when there is
	// one group -- to the Go type implementing it, as "dir.Type" with dir
	// relative to Dir:
	//
	//	Scaffold: map[string]string{"product": "internal/catalog.ProductResolver"}
	//
	// Missing methods are appended to dir/<group>.resolvers.go, created with the
	// type and a var _ Resolver assertion if the type does not exist yet. Methods
	// that exist are never touched, wherever they are declared; the package is
	// parsed, not loaded. Empty means no scaffolding.
	Scaffold map[string]string
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
	// FieldDirective names an SDL directive that forces a field to the
	// Resolver interface, and the boolean argument that turns it on. Empty
	// means none.
	//
	// gqlgen spells it @goField(forceResolver: true), and one real schema
	// carries 4 567 of them. It is not redundant with AutoBind refusing a
	// guess: AutoBind verifies that a struct field exists, and the author is
	// saying the value must be computed rather than read -- permission
	// filtering, a currency conversion, a field that is a column today and
	// will not be tomorrow. Binding it to the column anyway compiles and
	// answers the wrong thing, which is worse than not compiling.
	//
	// Other arguments gqlgen's directive takes (name:, omittable:) are not
	// read.
	//
	// Measured: on the 5 503-type schema it was built for it moved the resolver
	// count by exactly zero, because every field it forces was a computed field
	// the Go type does not have. It earns its place on the field that is a
	// column and that the author wants computed anyway; do not quote it as a
	// migration win.
	FieldDirective FieldDirective
	// Manifest binds GraphQL types and fields explicitly instead of letting
	// the generator infer them. It loads no Go type information; see the
	// Manifest documentation.
	Manifest *Manifest
	// AutoBind names package patterns to discover bindings from. Only those
	// packages are loaded, and only their export data: no syntax trees and no
	// function bodies. A Manifest entry overrides discovery field by field.
	AutoBind []string
	// Notef receives a line whenever the generator resolves a disagreement it
	// could have resolved another way -- today, a declared enum binding that
	// AutoBind can see would not compile, which is dropped so the enum is
	// modelled instead. A nil func discards them.
	//
	// It is not a log. Every line is a decision the author would want to know
	// about and can act on, which is why there is no level and no filtering:
	// anything that does not meet that bar belongs in an error or nowhere.
	Notef func(format string, args ...any)
	// ZeroForNullInputs emits graphql.ZeroForNull() on every generated Input
	// binding, so a Go field that cannot be null may back a nullable input
	// position and absent, null and the zero value all mean the same thing.
	//
	// It is one setting rather than a list because the schemas that need it
	// need it everywhere: an ORM emits filter inputs by the hundred with
	// `IsNil bool` under `Boolean`, and 11 035 such fields on one real schema
	// are not a list anyone maintains by hand. Read the ZeroForNull godoc
	// before setting it -- for a PATCH-style input, absent and null are
	// different requests and NullableInputOmittable is the option you want.
	//
	// **This is a migration aid, not a design.** The right long-term fix is in
	// the generator that emits the Go type: a nullable SDL field should be
	// backed by a pointer. Setting this teaches an engine invariant to tolerate
	// a generator's convention, and every schema that keeps it set keeps a
	// distinction the GraphQL specification makes and the Go type cannot.
	ZeroForNullInputs bool
	// NullableInputOmittable uses graphql.Omittable[*T] for nullable
	// input-object fields so PATCH-style inputs distinguish absent from
	// null. Field arguments stay pointers.
	NullableInputOmittable bool
}

// Generate writes bindings, models, argument structs and a Resolver
// interface from SDL. It loads no Go packages unless AutoBind names some, and
// then only their export data.
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
	// Written concurrently: each write first reads the old file to leave an
	// unchanged one alone, and with a package per group that is thousands of
	// opens, which on Windows was most of a generate.
	outDir := filepath.Join(dir, cfg.Output)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, 16)
	)
	for rel, src := range files {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := writeGo(filepath.Join(outDir, rel), src); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return b.scaffold(b.uniqueGroups())
}

// RootField is what Config.RootFieldGroup is told about a root field.
type RootField struct {
	// Root is the root type's name, usually Query, Mutation or Subscription.
	Root string
	// Name is the field's name.
	Name string
	// SDLFile is the file the field is declared in, which for a field added by
	// `extend type` is the extension's file and not the root's.
	SDLFile string
	// ReturnType is the named type the field returns, lists and non-null
	// removed.
	ReturnType string
	// ReturnGroup is the group ReturnType is in, or empty for a built-in
	// scalar, which belongs to no group.
	ReturnGroup string
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

// FieldDirective names an SDL directive that forces a field to the Resolver
// interface.
type FieldDirective struct {
	// Name is the directive, without the @.
	Name string
	// ForceResolverArg is the boolean argument that forces the resolver.
	ForceResolverArg string
}

// IsZero reports that no directive was named.
func (d FieldDirective) IsZero() bool { return d.Name == "" || d.ForceResolverArg == "" }
