package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	sdlparser "github.com/vektah/gqlparser/v2/parser"

	"github.com/syssam/graphql-go/fed"
	"github.com/syssam/graphql-go/internal/fedlink"
)

type builder struct {
	// scalarsNamedBefore is the custom scalars the last generate named, read
	// from its schema.go, and unboundScalars the ones this schema has: see
	// reportUnboundScalars.
	scalarsNamedBefore []string
	unboundScalars     []string

	// fedNames is how a federated schema spells the federation directives.
	fedNames fedlink.Spellings
	cfg      Config
	dir      string

	// wire is what Config.Wire needs of the dependencies struct and the
	// scaffold targets, read once. See wireFields and wireArg.
	wire struct {
		read    bool
		fields  map[string]*depsField
		targets map[string]int
		// ctors is, per target directory, each hand-written NewX there and
		// each alias the package declares, under its own name.
		ctors map[string]map[string]*constructor
		// generic is whether the dependencies type has type parameters.
		generic bool
		// named is every named field of the struct by its own name, exported
		// or not, and structRead whether the struct was found to read them
		// from. Wire.Pass names a field exactly.
		named      map[string]*depsField
		structRead bool
		// embeds is whether the struct embeds anything, in which case it has
		// promoted fields this run cannot list.
		embeds bool
		// dirs is each target directory by its lower-cased path, under the
		// spelling first met. See wireDir.
		dirs map[string]string
		// anyField is whether any group is handed a field of its own; nil
		// until asked.
		anyField *bool
	}
	schema   *ast.Schema
	sources  []*ast.Source
	pkgName  string
	modelImp string

	// modelSplit emits one model package per group instead of a single
	// shared one. Set by emit once the groups are known.
	modelSplit bool

	// manifest is nil unless Config.Manifest was given. Its type bindings are
	// already folded into cfg.Models, so only field kinds and group overrides
	// are read from here.
	manifest *manifest

	// namesByKind and groupByType memoize two answers that do not change once
	// the schema is loaded. Both are asked once per group, and emit runs per
	// group, so at 800 groups over 4 800 types they were the difference between
	// linear and quadratic: typeNames alone scanned and sorted every type on
	// each of its ten-odd call sites.
	namesByKind map[ast.DefinitionKind][]string
	groupByType map[string]string
	// groupByRootField memoizes rootFieldGroup, keyed "Root.field".
	groupByRootField map[string]string
	// fieldsByGroup indexes every object field by group, "" holding all of
	// them; groups memoizes uniqueGroups. Both are built in emit, after the
	// model map is final, and fieldScans and groupScans count the builds.
	fieldsByGroup map[string][]groupField
	groups        []string
	fieldScans    int
	groupScans    int
	// marshalers are the SDL enums and scalars whose declared Go type encodes
	// itself; they bind through EnumMarshaler and ScalarMarshaler. Only
	// AutoBind loads the types that can say so.
	marshalers map[string]bool

	// nameScans counts how often typeNames actually scanned, which is what
	// TestTypeNamesIsComputedOncePerKind reads: once per kind and not once
	// per group is the difference between linear and quadratic here.
	nameScans int

	// exprImports memoizes modelExprImports, which modelQualifier asks for
	// once per type reference while itself scanning every Models entry. With
	// AutoBind that map holds an entry per discovered type, so recomputing it
	// was references times types. setModels clears it; nothing else may write
	// cfg.Models.
	exprImports map[string]string
	// exprQualifiers is its inverse, import path to qualifier, filled with it.
	exprQualifiers map[string]string
	exprScans      int
	// checked is each file checkScaffoldTargets parsed, by path, for
	// indexPackage to take: the author's files are read before anything is
	// written and again to scaffold, and parsed once. It is taken only where
	// the file still holds the bytes parsed. In between, the run writes its
	// output, prunes, and scaffolds into the same directories, which a second
	// path may reach, so only the bytes say a parse is of the file as it is.
	checked map[string]*targetFile
	// scaffoldParses counts the files scaffold parses, which must be each
	// file once per run however many groups share a package.
	scaffoldParses int
	// pkgNames memoizes packageName until scaffolding is set, which scaffold
	// does: from there the run has written packages and goes on writing them.
	// pkgNameReads counts the asks that went to the files.
	pkgNames     map[string]string
	scaffolding  bool
	pkgNameReads int
	// scaffoldProbes counts how often checkScaffoldTargets asks the
	// filesystem where a directory is, which must grow with the directories
	// and not with targets times output files.
	scaffoldProbes int

	// loads counts calls to packages.Load. "It reads no Go type information
	// without AutoBind" is a promise the documentation makes and only the
	// `len(cfg.AutoBind) > 0` guard keeps; loading a module is the cost this
	// generator exists to avoid, so a regression here is the whole design
	// going quietly. TestNoPackagesAreLoadedWithoutAutoBind reads it.
	loads int

	// markers memoizes hasMarker. setModels clears it, because whether a
	// member is mapped is what decides it.
	markers map[string]bool

	// extraEnums holds the second Go type some SDL enums have, rendered as
	// the references generated code will use, keyed by SDL enum name. See
	// Manifest.ExtraEnums. extraImports carries the packages they need, which
	// neither Config.Models nor the generated model packages name.
	extraEnums   map[string][]extraEnum
	extraImports map[string]string
}

// notef reports a decision through Config.Notef, if there is one.
func (b *builder) notef(format string, args ...any) {
	if b.cfg.Notef != nil {
		b.cfg.Notef(format, args...)
	}
}

// setModels replaces the model map and drops what was memoized from it.
func (b *builder) setModels(models map[string]string) {
	b.cfg.Models = models
	b.exprImports = nil
	b.exprQualifiers = nil
	b.markers = nil
}

func newBuilder(dir string, cfg Config) (*builder, error) {
	var srcs []*ast.Source
	// A file two globs both name is one source: schema/**/*.graphql beside a
	// file under schema/ named outright would otherwise define its types twice.
	loaded := map[string]bool{}
	for _, glob := range cfg.SchemaGlobs {
		pattern := glob
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(dir, glob)
		}
		matches, err := sourceFilesMatching(pattern, filepath.Join(outputDir(dir, cfg.Output), "schema"))
		if err != nil {
			return nil, fmt.Errorf("codegen: glob %q: %w", glob, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("codegen: glob %q matched no files", glob)
		}
		for _, path := range matches {
			if loaded[path] {
				continue
			}
			loaded[path] = true
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			srcs = append(srcs, &ast.Source{Name: filepath.ToSlash(path), Input: string(raw)})
		}
	}
	load := srcs
	var fedNames fedlink.Spellings
	if cfg.Federation {
		// The directives as this schema spells them, which is how
		// fed.Subgraph reads it: under a link's namespace or an import's own
		// name as well as bare. A schema that does not parse says so below.
		directives := fed.Directives
		if doc, err := sdlparser.ParseSchemas(srcs...); err == nil {
			fedNames = fedlink.Read(doc, fed.Directives)
			directives += fedNames.Declare(fed.Directives)
		}
		// BuiltIn: the directives are the protocol's, not the author's, and
		// nothing is generated for them.
		load = append([]*ast.Source{{Name: "federation.graphql", Input: directives, BuiltIn: true}}, srcs...)
	}
	sch, err := gqlparser.LoadSchema(load...)
	if err != nil {
		return nil, fmt.Errorf("codegen: load schema: %w", err)
	}
	pkgName := cfg.Package
	if i := strings.LastIndex(pkgName, "/"); i >= 0 {
		pkgName = pkgName[i+1:]
	}
	b := &builder{
		cfg:      cfg,
		dir:      dir,
		schema:   sch,
		fedNames: fedNames,
		sources:  srcs,
		pkgName:  pkgName,
		modelImp: cfg.Package + "/model",
	}

	// Bindings the SDL already carries, folded in before the manifest so an
	// explicit Models entry still wins over what a directive says.
	if !cfg.ModelDirective.IsZero() {
		cfg.Models = foldModelDirective(sch, cfg.ModelDirective, cfg.Models)
		// foldModelDirective returns a new map, and discovery below reads
		// the builder's copy rather than this one. Without this line every
		// field whose type the SDL binds is measured against the model the
		// generator would have written instead, and none of them match.
		b.setModels(cfg.Models)
	}

	// Discovery produces a manifest and then stops, so everything downstream
	// is the manifest path. It runs against the builder as it stands, because
	// deciding whether a Go field can answer a GraphQL field means knowing
	// what Go type that field needs, which only goType can say. Discovery only
	// ever adds object-type mappings, so the leaf types it asks about are
	// already settled.
	explicit := cfg.Manifest
	if len(cfg.AutoBind) > 0 {
		discovered, aerr := autoBind(dir, cfg.AutoBind, sch, b)
		if aerr != nil {
			return nil, aerr
		}
		// The builder's map, not the local one: discovery drops a declared
		// binding it can see would not compile, and the local copy still
		// carries it.
		explicit = mergeManifests(yieldToDeclared(discovered, b.cfg.Models), explicit)
		cfg.Models = b.cfg.Models
	}

	// Folding the manifest's type bindings into cfg.Models here means model
	// references, imports and the mapped check keep working unchanged; the
	// manifest is only consulted afterwards for field kinds and groups.
	man, models, err := newManifest(explicit, sch, cfg.Models)
	if err != nil {
		return nil, fmt.Errorf("codegen: %w", err)
	}
	b.setModels(models)
	b.manifest = man
	b.setExtraEnums(explicit)
	if err := b.checkFieldNames(); err != nil {
		return nil, fmt.Errorf("codegen: %w", err)
	}
	if err := b.checkListDepth(); err != nil {
		return nil, err
	}
	if err := b.checkTagDirectives(); err != nil {
		return nil, fmt.Errorf("codegen: %w", err)
	}
	return b, nil
}

// typeNames returns the schema's type names of one kind, sorted. The slice is
// memoized and shared, so callers range over it and must not write to it.
func (b *builder) typeNames(kind ast.DefinitionKind) []string {
	if names, ok := b.namesByKind[kind]; ok {
		return names
	}
	var names []string
	for name, def := range b.schema.Types {
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if def.Kind == kind {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	b.nameScans++
	if b.namesByKind == nil {
		b.namesByKind = make(map[ast.DefinitionKind][]string, 6)
	}
	b.namesByKind[kind] = names
	return names
}

func (b *builder) isRoot(name string) bool {
	s := b.schema
	return (s.Query != nil && s.Query.Name == name) ||
		(s.Mutation != nil && s.Mutation.Name == name) ||
		(s.Subscription != nil && s.Subscription.Name == name)
}

// isSubscriptionRoot reports whether name is the subscription root, whose
// fields bind to a source stream rather than to a resolver.
func (b *builder) isSubscriptionRoot(name string) bool {
	s := b.schema
	return s.Subscription != nil && s.Subscription.Name == name
}

func (b *builder) named(t *ast.Type) *ast.Definition {
	if t == nil {
		return nil
	}
	return b.schema.Types[t.Name()]
}
