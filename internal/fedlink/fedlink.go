// Package fedlink reads how a subgraph's SDL spells the Apollo Federation
// directives.
//
// fed.Subgraph needs it to build the schema and gqlc needs it to generate
// one, and the two have to agree: a type that is an entity to the runtime and
// not to the generator is a resolver the generated NewSchema never asks for.
// It is here, and not in fed, so that both read one implementation without
// fed exporting what is only a tool's question.
package fedlink

import (
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// federationSpec prefixes the url of the @link that brings the federation
// directives into a subgraph. The version follows it.
const federationSpec = "https://specs.apollo.dev/federation/"

// federationNamespace is the name a link to the specification has when it
// gives none with as:, the last path segment of the url before the version.
const federationNamespace = "federation"

// Spellings is how one subgraph's SDL writes the federation directives.
//
// Federation 2 lets a schema choose. A directive that @link imports is
// written bare; one it does not import is written under the link's
// namespace, @federation__key; and an import may rename one,
// import: [{name: "@key", as: "@entity"}]. All three are the same directive
// to the router, so all three are here.
type Spellings struct {
	// renamed maps every spelling the prelude does not already declare to
	// the directive it stands for.
	renamed map[string]string
}

// Is reports whether a directive written name is the federation directive
// canonical.
func (s Spellings) Is(name, canonical string) bool {
	return name == canonical || s.renamed[name] == canonical
}

// Read collects the spellings a schema document's @link directives
// introduce. A link to any other specification is left alone: its imports
// are that specification's names, whatever they are called. directives is
// the SDL declaring the federation directives under their canonical names.
func Read(doc *ast.SchemaDocument, directives string) Spellings {
	declared := declarations(directives)
	s := Spellings{renamed: map[string]string{}}
	for _, defs := range []ast.SchemaDefinitionList{doc.Schema, doc.SchemaExtension} {
		for _, def := range defs {
			for _, link := range def.Directives.ForNames("link") {
				url := link.Arguments.ForName("url")
				if url == nil || url.Value == nil || !strings.HasPrefix(url.Value.Raw, federationSpec) {
					continue
				}
				namespace := federationNamespace
				if as := link.Arguments.ForName("as"); as != nil && as.Value != nil && as.Value.Raw != "" {
					namespace = as.Value.Raw
				}
				for canonical := range declared {
					s.renamed[namespace+"__"+canonical] = canonical
				}
				imports := link.Arguments.ForName("import")
				if imports == nil || imports.Value == nil {
					continue
				}
				for _, item := range imports.Value.Children {
					// A plain string imports a directive under its own name,
					// which the prelude declares already.
					if item.Value == nil || item.Value.Kind != ast.ObjectValue {
						continue
					}
					var name, as string
					for _, f := range item.Value.Children {
						if f.Value == nil {
							continue
						}
						switch f.Name {
						case "name":
							name = strings.TrimPrefix(f.Value.Raw, "@")
						case "as":
							as = strings.TrimPrefix(f.Value.Raw, "@")
						}
					}
					if _, ok := declared[name]; ok && as != "" && as != name {
						s.renamed[as] = name
					}
				}
			}
		}
	}
	return s
}

// Declare returns the directive declarations for the renamed spellings, each
// a copy of the one directives gives its canonical name, so a schema loader
// accepts @federation__key wherever it accepts @key.
func (s Spellings) Declare(directives string) string {
	if len(s.renamed) == 0 {
		return ""
	}
	declared := declarations(directives)
	names := make([]string, 0, len(s.renamed))
	for name := range s.renamed {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString("directive @" + name + declared[s.renamed[name]] + "\n")
	}
	return b.String()
}

// declarations reads directives into name -> the rest of its declaration,
// arguments and locations. @link is left out: it is what does the renaming,
// and the link specification, not this one, says how it is itself renamed.
func declarations(directives string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(directives, "\n") {
		rest, ok := strings.CutPrefix(line, "directive @")
		if !ok {
			continue
		}
		end := strings.IndexAny(rest, "( ")
		if end < 0 || rest[:end] == "link" {
			continue
		}
		out[rest[:end]] = rest[end:]
	}
	return out
}

// HasResolvableKey reports whether def carries a @key this subgraph resolves.
// @key(resolvable: false) names the key of an entity another subgraph owns:
// the router never asks this one for it, so such a type is not an entity
// here, needs no resolver and stays out of _Entity.
func HasResolvableKey(def *ast.Definition, names Spellings) bool {
	for _, d := range def.Directives {
		if !names.Is(d.Name, "key") {
			continue
		}
		if arg := d.Arguments.ForName("resolvable"); arg == nil || arg.Value == nil || arg.Value.Raw != "false" {
			return true
		}
	}
	return false
}
