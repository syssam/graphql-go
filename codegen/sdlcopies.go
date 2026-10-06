package codegen

import (
	"errors"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
)

// copyName is the name a schema source is copied into Output/schema under:
// its own, with the extension the generated root embeds. A source may be
// called anything -- gqlgen's are .graphqls -- and a copy that kept another
// extension was one the embed never saw.
func copyName(source string) string {
	return fileStem(filepath.FromSlash(source)) + ".graphql"
}

// foreignSchemaFiles refuses a generate that finds, in outDir/schema, an SDL
// file gqlc did not write. That directory is gqlc's: the root package embeds
// it by glob, and a copy whose source is gone has to go too. But a copy
// carries no header to know it by, and going by where a file is deleted one
// its author had put there -- SDL kept from before gqlc, a draft beside
// sources that are themselves read from that directory. It cannot be left
// either, since what is in the directory is in the schema, which leaves
// saying so and touching nothing.
func foreignSchemaFiles(outDir string, written map[string][]byte, recorded []string) error {
	_, foreign, err := schemaLeftovers(outDir, writtenKeys(outDir, written), recorded)
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range foreign {
		errs = append(errs, fmt.Errorf("codegen: %s is not a copy gqlc made: no schema source produces it and the last generate did not write it. It is in the directory the generated package embeds whole, so it can be neither left nor deleted unasked; add it to the schema globs, or move or delete it", filepath.ToSlash(path)))
	}
	return errors.Join(errs...)
}

// schemaLeftovers is the SDL files in outDir/schema this run did not write:
// stale, the ones the last run did, which are gqlc's to delete, and foreign,
// the rest. recorded is the last run's copies by name.
func schemaLeftovers(outDir string, keys writtenSet, recorded []string) (stale, foreign []string, err error) {
	dir := filepath.Join(outDir, "schema")
	matches, err := filepath.Glob(filepath.Join(dir, "*.graphql"))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range matches {
		base := filepath.Base(path)
		if keys.has(filepath.Join("schema", base)) {
			continue
		}
		// By name, or by a name the filesystem says is this file: a copy
		// written into an existing one of another letter case keeps the
		// spelling it had, where case is ignored.
		ours := slices.ContainsFunc(recorded, func(name string) bool {
			return name == base || (strings.EqualFold(name, base) && samePath(filepath.Join(dir, name), path))
		})
		if ours {
			stale = append(stale, path)
		} else {
			foreign = append(foreign, path)
		}
	}
	return stale, foreign, nil
}

// copiesListed opens the part of the sdl variable's comment that lists the
// copies, and scalarsListed the line of it that names the custom scalars.
// emitEmbed writes both and recordedIn finds the comment by the variable, so
// only the indentation of a line says it is a copy's name.
const (
	copiesListed  = "sdl is the schema, embedded from the copies gqlc made of its sources:"
	scalarsListed = "Custom scalars gqlc has said need a binding at NewSchema:"
)

// recordedIn is what a generate left in the schema.go it wrote for the next
// one: the copies it wrote into schema/, by name, and the custom scalars it
// named. Both are nil for output from before they were kept, for a file that
// does not parse and for one that is not there -- where no file is known to
// be gqlc's, and every scalar is new.
func recordedIn(schemaGo []byte) (copies, scalars []string) {
	if len(schemaGo) == 0 {
		return nil, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "schema.go", schemaGo, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil
	}
	for _, d := range f.Decls {
		gd, ok := d.(*goast.GenDecl)
		if !ok || gd.Tok != token.VAR || gd.Doc == nil || len(gd.Specs) != 1 {
			continue
		}
		if vs, ok := gd.Specs[0].(*goast.ValueSpec); !ok || len(vs.Names) != 1 || vs.Names[0].Name != "sdl" {
			continue
		}
		for line := range strings.SplitSeq(gd.Doc.Text(), "\n") {
			// An indented line of the comment is a copy; the prose is not.
			if name, listed := strings.CutPrefix(line, "\t"); listed && name != "" {
				copies = append(copies, name)
			} else if names, listed := strings.CutPrefix(line, scalarsListed); listed {
				scalars = strings.Fields(names)
			}
		}
		return copies, scalars
	}
	return nil, nil
}
