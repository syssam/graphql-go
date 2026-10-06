package main

import (
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// usage is what -h prints. Every key gqlc.yaml takes has a line here, nested
// ones included, so the help is a map of the whole command; the tests fail
// when a key or a flag is added without one, or a line passes 80 columns.
// cmd/gqlc/README.md has the detail.
func usage(fs *flag.FlagSet, w io.Writer) {
	// Help that cannot be written to stderr has nowhere else to go.
	_, _ = fmt.Fprint(w, `gqlc generates Go from a GraphQL schema: models, argument structs, a
Resolver interface for each group, and the bindings that serve them.

Usage:
  gqlc [-config gqlc.yaml]             generate from a configuration file
  gqlc -schema FILES -out DIR          generate with no configuration file
  gqlc -check [-config gqlc.yaml]      write nothing; fail naming what is stale
  gqlc -verify [-config gqlc.yaml]     generate, then go build what was written
  gqlc -version                        print which gqlc this is

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
	_, _ = fmt.Fprint(w, `
gqlc.yaml keys (cmd/gqlc/README.md describes each):
  schema                    SDL files or globs to read
  output                    directory to write into
  package                   import path of output
  preset                    gqlgen: sets modelDirective, fieldDirective,
                            tagDirective, jsonTags, structObjectFields and
                            inputListPointers as a schema from gqlgen needs
  models                    GraphQL type to Go type, for types not generated
  autoBind                  Go packages whose types answer the schema's
  modelDirective            SDL directive naming a type's Go type (@goModel)
    modelDirective.name     the directive, without @
    modelDirective.arg      its argument holding import/path.Type
  fieldDirective            SDL directive forcing a field to the Resolver
    fieldDirective.name     the directive, without @ (gqlgen: goField)
    fieldDirective.forceResolverArg
                            its boolean argument (gqlgen: forceResolver)
  tagDirective              SDL directive adding a struct tag to an input
                            object's field (gqlgen: @goTag)
    tagDirective.name       the directive, without @
    tagDirective.keyArg     its argument holding the tag key
    tagDirective.valueArg   its argument holding the tag value
  fieldNames                Go names for SDL fields and arguments, by name or
                            by coordinate (User.userId, Query.user(byId:))
  jsonTags                  json tags on generated model fields
  structObjectFields        object fields read from the model, as gqlgen did
  inputListPointers         []*T for lists of input objects, as gqlgen did
  nullableInputOmittable    graphql.Omittable for nullable input fields
  zeroForNullInputs         a non-nullable Go field may back a nullable input
  inlineAccessors           fields bound to model methods run inline
  inline                    resolver fields that run inline, as Type.field
  federation                make the schema an Apollo Federation subgraph
  groups                    group names from SDL file names:
    groups.trimPrefix       removed from the front of each file's name
    groups.rename           then old: new
  rootFields                a root field's group: file or returnType
  groupDir                  put group packages under output/<groupDir>
  scaffold                  group: dir.Type, to write missing Resolver stubs
  wire                      write wire.go, building every group from one value
    wire.deps               the dependencies type, as import/path.Type
    wire.pass               per group, what its constructor is given:
                            d, &d, d.Field or &d.Field
`)
}

// version is gqlc's own version and the Go it was built with.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "gqlc unknown " + runtime.Version()
	}
	return formatVersion(bi)
}

// formatVersion reads the graphql-go version out of build information. cmd/gqlc
// is in graphql-go's root module, so that module is always the main one:
// under go tool its version is the one go.mod requires, followed by what a
// replace directive points it at instead, since the required version is then
// not what was built. A build from a checkout already carries its commit,
// and "+dirty", in a pseudo-version; only a build that has none, such as go
// run's "(devel)", is given the commit from the VCS settings.
func formatVersion(bi *debug.BuildInfo) string {
	v := bi.Main.Version
	if v == "" {
		v = "unknown"
	}
	if r := bi.Main.Replace; r != nil {
		v += " => " + strings.TrimSpace(r.Path+" "+r.Version)
	}
	if bi.Main.Version == "(devel)" {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "+dirty"
				}
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" {
			v += " " + rev + dirty
		}
	}
	goVersion := bi.GoVersion
	if goVersion == "" {
		goVersion = runtime.Version()
	}
	return "gqlc " + v + " " + goVersion
}
