// Package blog is the composition root for the layered example. NewSchema is
// the only thing it exports, so every transport -- net/http, Echo, Fiber --
// wires the same schema without reaching into internal/.
package blog

//go:generate go tool gqlc -config gqlc.yaml

import (
	"context"
	"fmt"
	"strings"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog/graph"
	"github.com/syssam/graphql-go/examples/blog/internal/app"
	"github.com/syssam/graphql-go/examples/blog/internal/repository"
	resolver "github.com/syssam/graphql-go/examples/blog/internal/transport/graphql"
)

// NewSchema builds the schema over a freshly seeded store.
func NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	return newSchema(repository.NewStore(), opts...)
}

// newSchema keeps the store visible to this package's tests, which drive the
// executor and inspect the broker at the same time.
func newSchema(repo *repository.Store, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	all := append([]graphql.SchemaOption{
		graphql.Scalar("Time", marshalTime, unmarshalTime),
		graphql.Directive("upper", func(next graphql.FieldFunc) graphql.FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if s, ok := v.(string); ok {
					return strings.ToUpper(s), err
				}
				return v, err
			}
		}),
	}, opts...)
	return graph.NewSchema(resolver.New(app.New(repo)), all...)
}

func marshalTime(w *graphql.Writer, t time.Time) error {
	w.String(t.UTC().Format(time.RFC3339Nano))
	return nil
}

func unmarshalTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string, got %T", v)
	}
	return time.Parse(time.RFC3339Nano, s)
}
