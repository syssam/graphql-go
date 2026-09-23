package graphql

import (
	"context"
	"strings"
	"testing"
)

type missingJSON string
type missingColor string

// The error for an unbound leaf names the option to add and the Go type the
// field already returns. gqlc generates a Go type for each unmapped custom
// scalar but cannot know its wire format, so this is the first error a
// generated schema meets; "has no Scalar or Enum binding" left the fix to be
// worked out, and called an unbound enum a scalar.
func TestMissingLeafBindingNamesTheFix(t *testing.T) {
	for _, c := range []struct {
		name string
		sdl  string
		opt  SchemaOption
		want string
	}{
		{
			name: "scalar",
			sdl:  `scalar JSON type Query { meta: JSON }`,
			opt:  Query(Resolve("meta", func(context.Context, Root) (*missingJSON, error) { return nil, nil })),
			want: `scalar JSON has no binding; add graphql.Scalar[graphql.missingJSON]("JSON", marshal, unmarshal)`,
		},
		{
			name: "enum",
			sdl:  `enum Color { RED } type Query { c: [Color!]! }`,
			opt:  Query(Resolve("c", func(context.Context, Root) ([]missingColor, error) { return nil, nil })),
			want: `enum Color has no binding; add graphql.Enum[graphql.missingColor]("Color", map[graphql.missingColor]string{...})`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(c.sdl), c.opt)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v\nwant it to contain %s", err, c.want)
			}
		})
	}
}
