package codegen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// GroupDir is the directory, relative to Output, that holds one package
	// per group: "register" puts the user group's bindings and Resolver
	// interface in Output/register/user. Empty means Output itself. It lets
	// generated bindings, generated models (Output/model) and the resolvers
	// implementing them (Scaffold) each sit under one directory of their
	// own, while every group stays its own package -- which is what keeps a
	// one-group edit from recompiling the rest. With one group it has no
	// effect but is still checked, so the schema growing a second group is
	// not when a bad value is first reported.
	//
	// Changing it moves the generated packages, and the previous ones are
	// deleted; code you wrote that imports them keeps the old path, which the
	// compiler reports. Scaffold appends to such a file and never rewrites
	// what is there, imports included.
	//
	// Refused: a path leaving Output, the model or schema directory in any
	// letter case, a directory the go command ignores (testdata, vendor, a
	// name starting with "." or "_"), and one reaching into another gqlc run's
	// output root.
	GroupDir string
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
	//
	// dir may be the group's own generated package ("graph/product.Handler"),
	// so one entity is one package: the stubs then name the group's types
	// unqualified and do not import it. The author's code and the generated
	// code then share a namespace, so a Type the generator declares there,
	// such as Resolver, is refused, and so is a hand-written declaration of a
	// name the generator emits, naming the file it is in.
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
	// TagDirective names an SDL directive that adds a struct tag to an
	// input-object field, with the arguments holding the tag's key and value.
	// Empty means none.
	//
	// gqlgen spells it @goTag(key: "valid", value: "required,max=200"), and a
	// service that validates the decoded input with struct tags -- the
	// go-validator `valid` tag, go-playground's `validate` -- reads exactly
	// what the SDL says. Without this option every such tag is dropped and the
	// validator runs over a struct with nothing to validate; nothing fails,
	// because a missing tag is not an error. One real schema carries 5 899.
	//
	// Tags are written after the graphql tag, in the order the directives
	// appear. Only input-object fields take them: an output field is read, not
	// validated. A key or value the struct-tag syntax cannot carry, a key that
	// repeats on one field, and the reserved key "graphql" are errors rather
	// than silently-wrong tags.
	TagDirective TagDirective
	// FieldNames maps an SDL field or argument name to the Go identifier it gets, instead of the
	// name the generator derives. Empty means the derived name for every field.
	//
	// The derived name for a leading underscore keeps an X (`_lastUpdatedAt` is
	// XLastUpdatedAt): an underscore has no upper case, so dropping it would leave the field
	// unexported, and the X keeps `_x` and `x` on one type from colliding. A schema that carries
	// exactly one such name, with no sibling it can collide with, may prefer the plain name
	// gqlgen gave it and its callers already use: `_lastUpdatedAt: LastUpdatedAt`.
	//
	// An entry applies wherever the SDL name appears, as a field, an input field or an argument.
	// The value must be an exported Go identifier, and two names that end up identical on one
	// type are refused with their SDL coordinates, as any other collision is.
	FieldNames map[string]string
	// JSONTags writes a json struct tag on every generated model field: the field's GraphQL
	// name, with ",omitempty" when the field is nullable -- what gqlgen wrote.
	//
	// Code that reads the tag by reflection depends on it, and a missing tag is silent: a
	// struct-to-map helper keys its entries by it, a validator names the offending field by
	// it in the error a client sees, a masker finds a field to redact by it, and a setter table
	// matches an aggregate's operations by it. Each falls back to the Go field name or skips the
	// field, so the output is wrong rather than broken.
	//
	// Off by default, which leaves the generated structs untagged.
	JSONTags bool
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
	// InlineAccessors binds a field AutoBind resolved to a method on the model
	// that takes no GraphQL arguments -- Order.Customer(ctx), Order.Items(ctx)
	// -- with graphql.Inline(), so it runs in its parent's goroutine instead of
	// on one of its own.
	//
	// For an ORM with field collection these methods answer from edges loaded
	// with the parent, and a goroutine per list element was the largest cost
	// of a page: on a fifty-order page with each order's customer and items,
	// inline was 1.9x faster end to end. The price is that an accessor which
	// does query -- a parent loaded without collection -- queries once per row
	// in sequence rather than concurrently. Methods with arguments, such as a
	// Relay connection that may page per row, are left concurrent.
	InlineAccessors bool
	// Inline names fields, as schema coordinates ("Order.totalCents"), whose
	// resolver runs in its parent's goroutine: one the author knows answers
	// from memory, such as a computed field over an edge the ORM already
	// loaded. A coordinate naming no field fails generation.
	Inline []string
	// Federation makes the schema an Apollo Federation subgraph. The SDL is
	// parsed after fed.Directives, so @key and the rest need no declaration,
	// and the generated NewSchema takes the resolvers the router reaches
	// entities through -- NewSchema(entities []fed.Entity, opts...) -- and
	// serves _service and _entities through fed.SubgraphFS. ValidateSchema
	// stays argument-free: it supplies a placeholder resolver for every @key
	// type, since building never calls one.
	Federation bool
	// NullableInputOmittable uses graphql.Omittable[*T] for nullable
	// input-object fields so PATCH-style inputs distinguish absent from
	// null. Field arguments stay pointers.
	NullableInputOmittable bool
	// StructObjectFields makes a field of object, interface or union type that
	// takes no arguments a struct field, read from the model like a scalar, on
	// every type whose Go binding is not fully stated (a generated model, or a
	// hand-written one named only in Models). Without it such a field is a
	// Resolver method, which suits an ORM entity that loads relations lazily but
	// leaves a model built by hand nowhere to hold the nested value. A field with
	// arguments is always a resolver, and a type bound through a manifest or
	// AutoBind keeps the bindings it states. Models may then refer to each
	// other across groups; a cycle among them falls back to one shared model
	// package, as cyclic inputs do.
	StructObjectFields bool
	// InputListPointers spells a list of input objects []*T instead of []T, as
	// gqlgen did. Callers written against that shape test elements for nil and
	// dereference them; a value slice breaks every one. Scalar and enum lists are
	// unchanged.
	InputListPointers bool
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
	if err := checkGroupDir(cfg.GroupDir); err != nil {
		return err
	}
	dir := cfg.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	outDir := filepath.Join(dir, cfg.Output)
	// Before the schema is loaded: a refused GroupDir costs nothing to report.
	if err := checkGroupDirRoot(outDir, cfg.GroupDir); err != nil {
		return err
	}
	b, err := newBuilder(dir, cfg)
	if err != nil {
		return err
	}
	if err := b.checkInline(); err != nil {
		return err
	}
	files, err := b.emit()
	if err != nil {
		return err
	}
	if err := b.checkGroupRoots(outDir); err != nil {
		return err
	}
	// Written concurrently: each write first reads the old file to leave an
	// unchanged one alone, and with a package per group that is thousands of
	// opens, which on Windows was most of a generate.
	rels := slices.Sorted(maps.Keys(files))
	errs := make([]error, len(rels))
	boundedEach(len(rels), func(i int) {
		errs[i] = writeGo(filepath.Join(outDir, rels[i]), files[rels[i]])
	})
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := pruneGenerated(outDir, files, b.notef); err != nil {
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

// TagDirective names an SDL directive that adds a struct tag to an
// input-object field.
type TagDirective struct {
	// Name is the directive, without the @.
	Name string
	// KeyArg is the string argument holding the tag key.
	KeyArg string
	// ValueArg is the string argument holding the tag value.
	ValueArg string
}

// IsZero reports that no directive was named.
func (d TagDirective) IsZero() bool { return d.Name == "" || d.KeyArg == "" || d.ValueArg == "" }

// writtenSet is the files a run wrote, relative to Output.
type writtenSet struct {
	outDir string
	exact  map[string]bool
	folded map[string][]string // lower-cased path -> each spelling written
}

func writtenKeys(outDir string, written map[string][]byte) writtenSet {
	s := writtenSet{outDir: outDir, exact: make(map[string]bool, len(written)), folded: make(map[string][]string, len(written))}
	for rel := range written {
		rel = filepath.ToSlash(rel)
		s.exact[rel] = true
		s.folded[strings.ToLower(rel)] = append(s.folded[strings.ToLower(rel)], rel)
	}
	return s
}

// has reports whether rel is a file this run wrote: the same path, or the
// same file reached under a spelling that differs in case. On Windows and
// macOS a file written as register/user/generated.go into an existing
// Register/ is walked under that spelling, and an exact compare deleted what
// the run had just written. Folding case in the compare itself was wrong the
// other way: on Linux schema/User.graphql and schema/user.graphql are two
// files, and keeping the stale one embeds both. Only the filesystem knows, so
// it is asked.
func (s writtenSet) has(rel string) bool {
	rel = filepath.ToSlash(rel)
	if s.exact[rel] {
		return true
	}
	for _, w := range s.folded[strings.ToLower(rel)] {
		if samePath(filepath.Join(s.outDir, rel), filepath.Join(s.outDir, w)) {
			return true
		}
	}
	return false
}

// checkGroupDir refuses a GroupDir that would leave Output, land group
// packages where the model packages or the embedded SDL already are, or name
// a directory the go command ignores: there pruning is skipped, so a
// package left behind would never be deleted, and vendor and dot-names are
// not importable as written.
func checkGroupDir(dir string) error {
	if dir == "" {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(dir))
	// Compared without case: on Windows and macOS "Model" is the directory
	// the models are in, and the collision surfaces as an import cycle that
	// names nothing.
	lower := strings.ToLower(clean)
	switch {
	case filepath.IsAbs(dir) || filepath.VolumeName(dir) != "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/"):
		// VolumeName catches Windows' drive-relative "C:foo", which is not
		// absolute and would be joined into both a path and an import path.
		return fmt.Errorf("codegen: GroupDir %q must be a directory inside Output", dir)
	case !importPathElems(clean):
		// On Linux "C:foo" is a directory like any other, but the colon is no
		// more allowed in the import path it becomes than it is on Windows.
		return fmt.Errorf("codegen: GroupDir %q is not a portable import path: use letters, digits and . _ -, no trailing dot and no reserved device name", dir)
	case lower == "model" || strings.HasPrefix(lower, "model/") || lower == "schema" || strings.HasPrefix(lower, "schema/"):
		return fmt.Errorf("codegen: GroupDir %q is where the generated models or the embedded SDL go", dir)
	}
	for elem := range strings.SplitSeq(clean, "/") {
		if ignoredDir(elem) {
			return fmt.Errorf("codegen: GroupDir %q names %q, a directory the go command ignores", dir, elem)
		}
		// Only code under Output could import a group package beneath an
		// internal/, and the root the application calls is not the one that
		// registers it: the build fails with "use of internal package".
		if strings.EqualFold(elem, "internal") {
			return fmt.Errorf("codegen: GroupDir %q names %q: nothing outside Output could import the group packages", dir, elem)
		}
	}
	return nil
}

// importPathElems reports whether every element of a slash path is one an
// import path may carry and every filesystem can hold as spelled: a
// conservative subset of what the go command accepts. A trailing dot is
// dropped by Windows ("reg." is created as "reg" while the import keeps the
// dot), a reserved device name (con, nul, com1, ...) is not a directory there
// at all, and "~" is how Windows spells a short name.
func importPathElems(p string) bool {
	for elem := range strings.SplitSeq(p, "/") {
		if elem == "" || strings.HasSuffix(elem, ".") || windowsReserved(elem) {
			return false
		}
		for _, r := range elem {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)
			if !ok {
				return false
			}
		}
	}
	return true
}

// windowsReserved reports whether elem, before any extension, is a device
// name Windows reserves in every directory.
func windowsReserved(elem string) bool {
	base, _, _ := strings.Cut(strings.ToLower(elem), ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

// ignoredDir reports whether the go command skips a directory of this name.
func ignoredDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// checkGroupDirRoot refuses a GroupDir that reaches into another gqlc run's
// output root: that run would prune the group packages this one writes
// there, and this one skips pruning them, so each regenerate undoes the other.
func checkGroupDirRoot(outDir, groupDir string) error {
	if groupDir == "" {
		return nil
	}
	dir := outDir
	for elem := range strings.SplitSeq(filepath.ToSlash(filepath.Clean(groupDir)), "/") {
		dir = filepath.Join(dir, elem)
		if err := foreignDir(dir); err != nil {
			return fmt.Errorf("codegen: GroupDir %q: %w", groupDir, err)
		}
	}
	return nil
}

// foreignDir refuses a directory this run must not write into: another gqlc
// run's output root, where that run prunes what this one writes, or another
// module's, where an import path under Package names the other module. A
// path that does not exist yet is this run's to create.
func foreignDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is a file, not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return fmt.Errorf("%s is another module's (it has its own go.mod)", dir)
	}
	root, err := isGqlcRoot(dir)
	if err != nil {
		return err
	}
	if root {
		return fmt.Errorf("%s is another gqlc run's output -- another config's, or an earlier layout of this one, which is yours to move or delete", dir)
	}
	return nil
}

// checkGroupRoots refuses a group package directory that is itself another
// gqlc run's root or module: the two runs would overwrite and prune each
// other's files on every generate, with nothing naming the collision.
//
// The prune walk that follows probes these directories again. Skipping that
// for the ones checked here measured 940 ms against 969 ms at 300 entities,
// p=0.071 over 12 interleaved runs: not distinguishable, so not kept.
func (b *builder) checkGroupRoots(outDir string) error {
	groups := b.uniqueGroups()
	if len(groups) <= 1 {
		return nil
	}
	errs := make([]error, len(groups))
	boundedEach(len(groups), func(i int) {
		if err := foreignDir(filepath.Join(outDir, filepath.FromSlash(b.groupRel(groups[i])))); err != nil {
			errs[i] = fmt.Errorf("codegen: group %s: %w", groups[i], err)
		}
	})
	return errors.Join(errs...)
}

// generatedHeader opens every Go file the generator writes (builder.header).
// It is what marks a file as the generator's to delete.
const generatedHeader = generatedMarker + "\n"

const generatedMarker = "// Code generated by gqlc. DO NOT EDIT."

// pruneGenerated deletes what gqlc owns under outDir and this run did not
// write, then any directory that deleting it left empty. It owns two kinds of
// file. Go files carrying generatedHeader: a group whose types all moved to
// Go bindings of their own stopped getting a models.go, and the old one
// stayed, compiling, so nothing said it was dead. And the SDL copies in
// outDir/schema, which the root package embeds by glob: a copy whose source
// was deleted or renamed stayed embedded and its types kept answering.
//
// Files without the header -- hand-written ones, scaffolded resolvers,
// another generator's output -- are never touched. Neither is a directory the
// go command itself ignores (testdata, vendor, and names starting with "." or
// "_"), where a golden copy of gqlc output is a fixture, not a stale file; nor
// another gqlc run's output nested in this one's.
//
// A Go file it cannot delete -- held open by an editor, read-only -- is named
// through notef and left: it is dead code that still compiles, and failing the
// generate there left scaffolding undone and every later run failing. An SDL
// copy it cannot delete fails the generate: the root embeds every copy, so the
// types it holds would go on being served, and a library caller with no Notef
// would never hear of it.
func pruneGenerated(outDir string, written map[string][]byte, notef func(string, ...any)) error {
	keys := writtenKeys(outDir, written)
	// The SDL copies by glob, which follows a linked Output or schema/ the way
	// the embed reading them does; a walk does not.
	stale, err := staleSchemaCopies(outDir, keys)
	if err != nil {
		return err
	}
	// Walked from Output's real path, for the same reason: WalkDir does not
	// follow a link at its root, and a linked Output was pruned of nothing.
	root := outDir
	if real, err := filepath.EvalSymlinks(outDir); err == nil {
		root = real
	}
	var candidates []string
	if err := filepath.WalkDir(root, pruneWalker(root, keys, &candidates)); err != nil {
		return err
	}
	ours, err := generatedAmong(candidates)
	if err != nil {
		return err
	}
	for _, f := range removeStale(root, ours) {
		notef("could not delete %s, which this run no longer generates: %v", f.path, f.err)
	}
	var errs []error
	for _, f := range removeStale(root, stale) {
		errs = append(errs, fmt.Errorf("codegen: %s is no longer generated but is still embedded, and could not be deleted: %w", f.path, f.err))
	}
	return errors.Join(errs...)
}

// staleSchemaCopies is the SDL copies in outDir/schema this run did not write.
func staleSchemaCopies(outDir string, keys writtenSet) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(outDir, "schema", "*.graphql"))
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, path := range matches {
		if !keys.has(filepath.Join("schema", filepath.Base(path))) {
			stale = append(stale, path)
		}
	}
	return stale, nil
}

// removeFailure is a stale path removeStale could not delete, and why.
type removeFailure struct {
	path string
	err  error
}

// removeStale deletes each path, then each directory that leaves empty up to
// root, and returns what it could not delete; one failure does not stop the
// rest. The caller decides what a failure means.
func removeStale(root string, stale []string) []removeFailure {
	var failed []removeFailure
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failed = append(failed, removeFailure{path, err})
			}
			continue
		}
		// os.Remove refuses a directory that is not empty, which is the check.
		for dir := filepath.Dir(path); dir != root && os.Remove(dir) == nil; dir = filepath.Dir(dir) {
		}
	}
	return failed
}

// pruneWalker is pruneGenerated's walk: it decides which directories to
// enter, and appends to candidates each Go file this run did not write, whose
// header decides.
func pruneWalker(outDir string, keys writtenSet, candidates *[]string) fs.WalkDirFunc {
	return func(path string, d fs.DirEntry, err error) error {
		// Something removed while walking -- another generate, an editor's
		// temporary directory -- is skipped, not the end of the walk: stopping
		// there left every stale file after it in place and reported success.
		// So is what this process may not read (skippable).
		if skippable(err) {
			if d != nil && d.IsDir() && path != outDir {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == outDir {
				return nil
			}
			if ignoredDir(d.Name()) {
				return filepath.SkipDir
			}
			// Another module's tree -- what an Output of "." walks into -- is
			// that module's, as the go command treats it.
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			nested, err := isGqlcRoot(path)
			switch {
			case skippable(err), nested:
				return filepath.SkipDir
			case err != nil:
				return err
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		if !keys.has(rel) {
			*candidates = append(*candidates, path)
		}
		return nil
	}
}

// generatedAmong is the paths whose file opens with gqlc's header, read
// sixteen at a time as the writes are: in a layout with a hand-written
// resolver beside every group, each is opened here, and serially that was
// most of the prune. A file gone before it is read is skipped.
func generatedAmong(paths []string) ([]string, error) {
	ours := make([]bool, len(paths))
	errs := make([]error, len(paths))
	boundedEach(len(paths), func(i int) {
		ok, err := hasGeneratedHeader(paths[i])
		if skippable(err) {
			return
		}
		ours[i], errs[i] = ok, err
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var out []string
	for i, p := range paths {
		if ours[i] {
			out = append(out, p)
		}
	}
	return out, nil
}

// ioParallel is how many files Generate opens at once, writing its output and
// reading headers to prune: enough to overlap Windows' per-open cost, few
// enough not to exhaust file handles.
const ioParallel = 16

// boundedEach calls fn for 0..n-1 on at most ioParallel goroutines, and
// waits. Workers take the next index as they finish: a goroutine per item,
// parked on a semaphore, was thousands of them at 800 groups for no more
// parallelism than this.
func boundedEach(n int, fn func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(n, ioParallel) {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		})
	}
	wg.Wait()
}

// skippable is an error pruning steps around rather than fails on: an entry
// gone since it was listed, or one this process may not read. Pruning is
// cleanup after the output is written; an unreadable directory elsewhere in
// Output holds nothing it could delete, and failing the whole generate there
// left scaffolding undone and every later run failing the same way.
func skippable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// isGqlcRoot reports whether dir is the output root of a gqlc run: its own
// embedded schema/ directory beside a gqlc-generated schema.go. Either alone
// is not enough -- a group package can hold a hand-written schema/ of its
// own, and skipping it would leave that group's stale files forever.
func isGqlcRoot(dir string) (bool, error) {
	fi, err := os.Stat(filepath.Join(dir, "schema"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	ours, err := hasGeneratedHeader(filepath.Join(dir, "schema.go"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return ours, err
}

// hasGeneratedHeader reports whether path opens with generatedMarker on a
// line of its own, reading only that much of it.
func hasGeneratedHeader(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	buf := make([]byte, len(generatedMarker)+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return isGeneratedSource(buf[:n]), nil
}

// isGeneratedSource reports whether src opens with generatedMarker on a line
// of its own. The line may end in \r\n: gqlc writes \n, but a checkout with
// core.autocrlf turns it into \r\n, and a file that stopped matching there
// would silently never be pruned.
func isGeneratedSource(src []byte) bool {
	if len(src) <= len(generatedMarker) || string(src[:len(generatedMarker)]) != generatedMarker {
		return false
	}
	end := src[len(generatedMarker)]
	return end == '\n' || end == '\r'
}
