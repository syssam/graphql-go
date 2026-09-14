package graphql

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/vektah/gqlparser/v2/ast"
)

// Source supplies SDL to NewSchema.
type Source struct {
	files    []*ast.Source
	fsys     fs.FS
	patterns []string
	parts    []Source
}

// SDL returns a Source holding one SDL document.
func SDL(s string) Source {
	return Source{files: []*ast.Source{{Name: "schema.graphql", Input: s}}}
}

// SDLBytes returns a Source holding one SDL document.
func SDLBytes(b []byte) Source {
	return SDL(string(b))
}

// SDLFS returns a Source that reads every file in fsys matching one of the
// glob patterns. Files are read when NewSchema runs and keep their names for
// error positions.
func SDLFS(fsys fs.FS, patterns ...string) Source {
	return Source{fsys: fsys, patterns: patterns}
}

// Sources concatenates several sources. Each fs.FS is read independently,
// so SDLFS from different file systems can be combined.
func Sources(srcs ...Source) Source {
	return Source{parts: srcs}
}

func (s Source) load() ([]*ast.Source, error) {
	if len(s.parts) > 0 {
		var out []*ast.Source
		for _, p := range s.parts {
			files, err := p.load()
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
		}
		if len(out) == 0 {
			return nil, errors.New("graphql: no SDL sources provided")
		}
		return out, nil
	}
	out := append([]*ast.Source(nil), s.files...)
	if s.fsys != nil {
		for _, pattern := range s.patterns {
			matches, err := fs.Glob(s.fsys, pattern)
			if err != nil {
				return nil, fmt.Errorf("graphql: glob %q: %w", pattern, err)
			}
			for _, name := range matches {
				b, err := fs.ReadFile(s.fsys, name)
				if err != nil {
					return nil, fmt.Errorf("graphql: read %s: %w", name, err)
				}
				out = append(out, &ast.Source{Name: name, Input: string(b)})
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("graphql: no SDL sources provided")
	}
	return out, nil
}
