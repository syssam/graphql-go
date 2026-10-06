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
// Either takes -check, which generates in memory, writes nothing, and fails
// naming every file a generate would create, rewrite or delete -- the "is
// the generated code current" step of a CI job or a pre-commit hook:
//
//	gqlc -config gqlc.yaml -check
//
// gqlc -h lists every gqlc.yaml key; the README describes each.
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
	"strings"

	"github.com/syssam/graphql-go/codegen"
	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	Schema                 []string `yaml:"schema"`
	Output                 string   `yaml:"output"`
	Package                string   `yaml:"package"`
	NullableInputOmittable bool     `yaml:"nullableInputOmittable"`
	// preset turns on a set of the keys below together, each of which a key
	// written out beside it still overrides. The one preset is gqlgen, for a
	// schema that arrives from it:
	//
	//	preset: gqlgen
	//
	// is modelDirective goModel/model, fieldDirective goField/forceResolver,
	// tagDirective goTag/key/value, jsonTags, inputListPointers and
	// structObjectFields. Each is silent when missing -- the output compiles
	// and the models are a different shape -- which is what makes a list of
	// six to remember worth one word.
	Preset string `yaml:"preset"`
	// StructObjectFields reads object-typed fields that take no arguments from
	// the model's struct, as gqlgen did, instead of a Resolver method. See
	// codegen.Config.StructObjectFields. A pointer, like the two below, so a
	// key written as false is told apart from one left for the preset.
	StructObjectFields *bool `yaml:"structObjectFields"`
	// InputListPointers spells a list of input objects []*T, as gqlgen did. See
	// codegen.Config.InputListPointers.
	InputListPointers *bool `yaml:"inputListPointers"`
	// zeroForNullInputs lets a Go field that cannot be null back a nullable
	// input position; read the graphql.ZeroForNull godoc before setting it.
	ZeroForNullInputs bool `yaml:"zeroForNullInputs"`
	// inlineAccessors runs fields bound to an argument-less model method
	// inline; see codegen.Config.InlineAccessors.
	InlineAccessors bool `yaml:"inlineAccessors"`
	// federation makes the schema an Apollo Federation subgraph; see
	// codegen.Config.Federation.
	Federation bool `yaml:"federation"`
	// inline lists fields ("Order.totalCents") whose resolver runs inline.
	Inline []string          `yaml:"inline"`
	Models map[string]string `yaml:"models"`
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
	// tagDirective adds a struct tag to an input-object field, for the validation tags
	// gqlgen's @goTag(key: "valid", value: "required") carried:
	//
	//	tagDirective:
	//	  name: goTag
	//	  keyArg: key
	//	  valueArg: value
	TagDirective struct {
		Name     string `yaml:"name"`
		KeyArg   string `yaml:"keyArg"`
		ValueArg string `yaml:"valueArg"`
	} `yaml:"tagDirective"`
	// jsonTags writes json:"name" (",omitempty" when nullable) on every generated model field,
	// as gqlgen did, for code that reads the tag by reflection.
	JSONTags *bool `yaml:"jsonTags"`
	// fieldNames overrides the Go name derived for an SDL field or argument name, for example
	// the plain name gqlgen gave a leading-underscore field:
	//
	//	fieldNames:
	//	  _lastUpdatedAt: LastUpdatedAt
	FieldNames map[string]string `yaml:"fieldNames"`
	// autoBind binds SDL types to existing Go types found in these package
	// patterns, instead of generating models for them:
	//
	//	autoBind: [./ent]
	AutoBind []string `yaml:"autoBind"`
	// groups names the group each SDL file becomes. The file stem is the
	// name; trimPrefix is removed from it first, and rename then maps what is
	// left, so an ORM's velox_product.graphql and its shared schema.graphql
	// can be product and root:
	//
	//	groups:
	//	  trimPrefix: velox_
	//	  rename: { schema: root }
	Groups struct {
		TrimPrefix string            `yaml:"trimPrefix"`
		Rename     map[string]string `yaml:"rename"`
	} `yaml:"groups"`
	// rootFields decides the group of each Query, Mutation and Subscription
	// field: "file" (the default) is the file declaring it, "returnType" the
	// group of the type it returns, for a generator that declares every root
	// field in one shared file. A field returning a scalar keeps its file's.
	RootFields string `yaml:"rootFields"`
	// scaffold writes a stub for every Resolver method an implementation does
	// not have yet, into dir/<group>.resolvers.go; see codegen.Config.Scaffold.
	//
	//	scaffold:
	//	  product: internal/catalog.ProductResolver
	//
	// The key "*" stands for every group no other key names, with {group}
	// where the name goes: "*": graph/{group}.Handler.
	Scaffold map[string]string `yaml:"scaffold"`
	// groupDir puts each group's package under output/<groupDir>/<group>
	// rather than output/<group>; see codegen.Config.GroupDir.
	//
	//	groupDir: register
	GroupDir string `yaml:"groupDir"`
	// wire writes output/wire.go: Wire(d), which builds every group's
	// Resolver from one dependencies value and returns their bindings for
	// NewSchema. Each group needs a scaffold target; see codegen.Config.Wire.
	//
	//	wire:
	//	  deps: example.com/app/internal/deps.Deps
	Wire struct {
		Deps string `yaml:"deps"`
		// pass says what a group's constructor is given where gqlc's own
		// reading is wrong: the scaffold key, then d, &d, d.Field or
		// &d.Field. See codegen.Wire.Pass.
		Pass map[string]string `yaml:"pass"`
	} `yaml:"wire"`
}

// applyPreset fills in what the preset stands for wherever the file left a
// key unset, so a key written out beside the preset decides.
func (fc *fileConfig) applyPreset() error {
	switch fc.Preset {
	case "":
		return nil
	case "gqlgen":
	default:
		return fmt.Errorf("preset: %q is not a preset; the one there is is gqlgen", fc.Preset)
	}
	// Field by field: a directive key written in part keeps what it says and
	// takes the rest, where filling in only an absent name threw a written
	// argument away and left a half-written directive switched off.
	for _, d := range []struct {
		key *string
		is  string
	}{
		{&fc.ModelDirective.Name, "goModel"}, {&fc.ModelDirective.Arg, "model"},
		{&fc.FieldDirective.Name, "goField"}, {&fc.FieldDirective.ForceResolverArg, "forceResolver"},
		{&fc.TagDirective.Name, "goTag"}, {&fc.TagDirective.KeyArg, "key"}, {&fc.TagDirective.ValueArg, "value"},
	} {
		if *d.key == "" {
			*d.key = d.is
		}
	}
	on := true
	for _, key := range []**bool{&fc.JSONTags, &fc.InputListPointers, &fc.StructObjectFields} {
		if *key == nil {
			*key = &on
		}
	}
	return nil
}

// set reports a boolean key's value, false when the file did not give one.
func set(b *bool) bool { return b != nil && *b }

// groupFunc turns the groups key into a codegen.GroupFunc, or nil when the
// key is absent, so the generator's own default stays in charge.
func (fc *fileConfig) groupFunc() func(typeName, sdlFile string) string {
	g := fc.Groups
	if g.TrimPrefix == "" && len(g.Rename) == 0 {
		return nil
	}
	return func(_, sdlFile string) string {
		stem := strings.TrimSuffix(filepath.Base(sdlFile), filepath.Ext(sdlFile))
		stem = strings.TrimPrefix(stem, g.TrimPrefix)
		if to, ok := g.Rename[stem]; ok {
			return to
		}
		return stem
	}
}

func (fc *fileConfig) rootFieldGroup() (func(codegen.RootField) string, error) {
	switch fc.RootFields {
	case "", "file":
		return nil, nil
	case "returnType":
		return func(f codegen.RootField) string { return f.ReturnGroup }, nil
	}
	return nil, fmt.Errorf("rootFields: %q is not file or returnType", fc.RootFields)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error { return runTo(args, os.Stdout, os.Stderr) }

// options is what the flags say.
type options struct {
	config, out, pkg             string
	schema, models               repeatable
	nullableOmittable, zeroNulls bool
	check, version, verify       bool
}

// newFlags is the command's flag set, apart so that a test can list every
// flag the help has to show.
func newFlags(stderr io.Writer) (*flag.FlagSet, *options) {
	o := &options{}
	fs := flag.NewFlagSet("gqlc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(fs, stderr) }
	fs.StringVar(&o.config, "config", "gqlc.yaml", "path to gqlc.yaml")
	fs.BoolVar(&o.check, "check", false, "write nothing; fail naming each file a generate would change")
	fs.BoolVar(&o.verify, "verify", false, "go build the output once written; fail with what the compiler says")
	fs.BoolVar(&o.version, "version", false, "print the graphql-go version gqlc was built from")
	fs.Var(&o.schema, "schema", "SDL file or glob, repeated or comma-separated; the flag form")
	fs.StringVar(&o.out, "out", "", "output directory for generated Go")
	fs.StringVar(&o.pkg, "pkg", "", "import path of -out; read from the nearest go.mod when empty")
	fs.Var(&o.models, "model", "NAME=go/import/path.Type, repeated or comma-separated")
	fs.BoolVar(&o.nullableOmittable, "nullable-input-omittable", false, "give nullable input fields Omittable")
	fs.BoolVar(&o.zeroNulls, "zero-for-null-inputs", false, "let a non-null Go field back a nullable input (see ZeroForNull)")
	return fs, o
}

// runTo is run with its output given, so a test can read what -h, -version
// and the notes print.
func runTo(args []string, stdout, stderr io.Writer) error {
	fs, o := newFlags(stderr)
	if err := fs.Parse(args); err != nil {
		// The usage has been printed, which is what was asked for.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// A stray argument is a mistyped flag or a forgotten -schema; ignoring it
	// generates from gqlc.yaml instead of from what was named.
	if fs.NArg() > 0 {
		return fmt.Errorf("gqlc: unexpected argument %q; schema files are passed with -schema", fs.Arg(0))
	}
	if o.version {
		_, err := fmt.Fprintln(stdout, version())
		return err
	}
	notef := func(format string, args ...any) {
		// A check that passes says nothing, so that it can stand in a CI job
		// or a pre-commit hook; the notes are a generate's to make.
		if o.check {
			return
		}
		// A note that cannot be written to stderr has nowhere else to go.
		_, _ = fmt.Fprintf(stderr, "gqlc: "+format+"\n", args...)
	}

	if len(o.schema) > 0 {
		explicitConfig := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				explicitConfig = true
			}
		})
		if explicitConfig {
			return errors.New("gqlc: -config and -schema are two ways to say the same thing; pass one")
		}
		return runFlags(o.schema, o.out, o.pkg, o.models, o.nullableOmittable, o.zeroNulls, o.check, o.verify, notef)
	}

	configPath := o.config
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("gqlc: read %s: %w", configPath, err)
	}
	// KnownFields, because a misspelled key is otherwise ignored and the
	// generator quietly does the default thing: nullableInputOmitable with one
	// t produces models with no Omittable and no hint why.
	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("gqlc: parse %s: %w", configPath, err)
	}
	rootFieldGroup, err := fc.rootFieldGroup()
	if err != nil {
		return fmt.Errorf("gqlc: %s: %w", configPath, err)
	}
	if err := fc.applyPreset(); err != nil {
		return fmt.Errorf("gqlc: %s: %w", configPath, err)
	}
	cfg := codegen.Config{
		AutoBind:               fc.AutoBind,
		GroupFunc:              fc.groupFunc(),
		RootFieldGroup:         rootFieldGroup,
		Scaffold:               fc.Scaffold,
		GroupDir:               fc.GroupDir,
		Dir:                    filepath.Dir(configPath),
		SchemaGlobs:            fc.Schema,
		Output:                 fc.Output,
		Package:                fc.Package,
		Models:                 fc.Models,
		NullableInputOmittable: fc.NullableInputOmittable,
		StructObjectFields:     set(fc.StructObjectFields),
		InputListPointers:      set(fc.InputListPointers),
		ZeroForNullInputs:      fc.ZeroForNullInputs,
		InlineAccessors:        fc.InlineAccessors,
		Inline:                 fc.Inline,
		Federation:             fc.Federation,
		ModelDirective:         codegen.ModelDirective{Name: fc.ModelDirective.Name, Arg: fc.ModelDirective.Arg},
		FieldDirective:         codegen.FieldDirective{Name: fc.FieldDirective.Name, ForceResolverArg: fc.FieldDirective.ForceResolverArg},
		TagDirective:           codegen.TagDirective{Name: fc.TagDirective.Name, KeyArg: fc.TagDirective.KeyArg, ValueArg: fc.TagDirective.ValueArg},
		JSONTags:               set(fc.JSONTags),
		Wire:                   codegen.Wire{Deps: fc.Wire.Deps, Pass: fc.Wire.Pass},
		FieldNames:             fc.FieldNames,
		Check:                  o.check,
		Verify:                 o.verify,
		Notef:                  notef,
	}
	return codegen.Generate(context.Background(), cfg)
}

// runFlags is the config-file-free path. Dir stays the process working
// directory, so -schema and -out are read the way a shell would read them.
func runFlags(schema repeatable, out, pkg string, models repeatable, nullableOmittable, zeroForNull, check, verify bool, notef func(string, ...any)) error {
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
		Check:                  check,
		Verify:                 verify,
		Notef:                  notef,
	})
}
