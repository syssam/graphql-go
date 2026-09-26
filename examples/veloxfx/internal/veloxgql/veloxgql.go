// Package veloxgql lets velox's generated CollectFields and Paginate read
// the selection from this engine instead of gqlgen, so a connection
// eager-loads the edges its nodes select, projects the columns they read and
// skips COUNT(*) when totalCount is not asked for.
//
// velox asks for the selection through gqlrelay.SelectedField, and this is
// that interface over graphql.Selection. It imports both libraries, which is
// why it lives beside the service rather than in either of them.
package veloxgql

import (
	"context"

	"github.com/syssam/velox/contrib/graphql/gqlrelay"

	graphql "github.com/syssam/graphql-go"
)

// Collect is the OperationInterceptor that puts the source on every
// operation's context. Register it with graphql.WithOperationInterceptor.
var Collect graphql.OperationInterceptor = graphql.OperationInterceptorFunc(
	func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
		return next(gqlrelay.WithSelectionSource(ctx, source), oc)
	})

// source answers for the resolver executing in ctx. Only fields beneath the
// resolved one are asked for their arguments, so the resolved field itself
// carries none: its own were decoded into the resolver's argument struct.
func source(ctx context.Context) (gqlrelay.SelectedField, bool) {
	fc := graphql.FieldFrom(ctx)
	if fc == nil {
		return nil, false
	}
	var vars map[string]any
	if oc := graphql.OperationFrom(ctx); oc != nil {
		vars = oc.Variables
	}
	return field{name: fc.Field.Name, sel: fc.Selection(), vars: vars}, true
}

type field struct {
	name string
	args map[string]any
	sel  graphql.Selection
	vars map[string]any
}

func (f field) FieldName() string         { return f.name }
func (f field) Arguments() map[string]any { return f.args }

// Fields returns the selection beneath f. With satisfies it is what applies
// to those types: velox passes an object type together with the interfaces
// it implements, and the plan keys an abstract selection by object type, so
// the interface names find nothing and the object's fields are the answer.
// Two names can yield the same response key, and a concrete selection
// answers every name with itself, so keys are kept once.
func (f field) Fields(satisfies []string) []gqlrelay.SelectedField {
	var (
		out  []gqlrelay.SelectedField
		seen map[string]bool
	)
	add := func(s graphql.Selection) {
		for sf := range s.Fields() {
			if seen != nil {
				if seen[sf.Alias] {
					continue
				}
				seen[sf.Alias] = true
			}
			// A malformed variable fails the field when it resolves, with
			// the error the client should see; planned without arguments,
			// the worst case is an edge loaded that nothing then reads.
			args, _ := sf.ArgumentMap(f.vars)
			out = append(out, field{name: sf.Name, args: args, sel: sf.Selection(), vars: f.vars})
		}
	}
	if len(satisfies) == 0 {
		add(f.sel)
		return out
	}
	seen = map[string]bool{}
	for _, typ := range satisfies {
		add(f.sel.ForType(typ))
	}
	return out
}

// NodeSelects reports whether the connection being resolved in ctx selects
// name on its nodes, edges { node { name } }, under any alias. A resolver
// uses it to load what a hand-written field reads and velox cannot know of.
func NodeSelects(ctx context.Context, name string) bool {
	for edges := range graphql.SelectionFrom(ctx).Fields() {
		if edges.Name != "edges" {
			continue
		}
		for node := range edges.Selection().Fields() {
			if node.Name == "node" && node.Selection().Has(name) {
				return true
			}
		}
	}
	return false
}
