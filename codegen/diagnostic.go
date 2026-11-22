package codegen

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// Diagnostic is a refusal anchored to a place in the SDL. The error Generate,
// Check or Render returns may hold several, joined; Diagnostics finds them.
//
// Error renders the position first, as file:line:column: , which editors,
// terminals and CI annotations turn into a link, followed by the message a
// refusal without a position would carry.
type Diagnostic struct {
	// File is the SDL file, relative to Config.Dir when it is under it and
	// absolute otherwise, with forward slashes.
	File string
	// Line and Column are 1-based. Column counts runes, as gqlparser does.
	Line, Column int
	// Coord is the schema coordinate refused: Type or Type.field.
	Coord string
	// Message is the refusal, without the position.
	Message string
}

func (d *Diagnostic) Error() string {
	if d.File == "" {
		return d.Message
	}
	return fmt.Sprintf("%s:%d:%d: %s", d.File, d.Line, d.Column, d.Message)
}

// Diagnostics returns every Diagnostic in err's tree, in the order the
// refusals were made: through errors.Join and through %w.
func Diagnostics(err error) []*Diagnostic {
	var out []*Diagnostic
	var walk func(error)
	walk = func(err error) {
		// A walk of the tree itself, one node at a time: errors.As would stop
		// at the first Diagnostic and miss the rest of a join.
		switch e := err.(type) { //nolint:errorlint // this is the unwrapping
		case nil:
		case *Diagnostic:
			out = append(out, e)
		case interface{ Unwrap() []error }:
			for _, inner := range e.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(e.Unwrap())
		}
	}
	walk(err)
	return out
}

// diag is a refusal of coord at pos. A position from a built-in source -- the
// federation directives gqlc adds -- is not the author's to open, and is left
// out, as is a missing one.
func (b *builder) diag(pos *ast.Position, coord string, format string, args ...any) error {
	d := &Diagnostic{Coord: coord, Message: fmt.Sprintf(format, args...)}
	if pos != nil && pos.Src != nil && !pos.Src.BuiltIn && pos.Src.Name != "" {
		d.File, d.Line, d.Column = b.sdlName(pos.Src.Name), pos.Line, pos.Column
	}
	return d
}

// sdlName is how a Diagnostic names an SDL file: relative to Config.Dir when
// it is under it, since that is where gqlc is run from. A source is named as
// its glob was joined onto Dir, so with a relative Dir its name is relative
// to the working directory, and is made absolute the same way Dir is before
// the two are compared: compared as given, Dir "sub" named the file
// sub/schema/s.graphql and the same Dir spelled absolutely schema/s.graphql.
func (b *builder) sdlName(name string) string {
	path := filepath.FromSlash(name)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if base, err := filepath.Abs(b.dir); err == nil {
		if rel, err := filepath.Rel(base, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
