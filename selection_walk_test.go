package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Abstract expansion is memoized into a DAG, so a plan reaches the same
// *selectionSet from several parents and every walk inside this package
// memoizes on it (authz_shape.walk, queryCostMemo). Selection is the same
// plan handed to an interceptor, and an interceptor cannot memoize on
// *selectionSet because it is unexported -- so if Selection exposed the DAG,
// a walk over it would be exponential in the number of abstract levels and
// there would be nothing the author could do about it.
//
// It does not: Fields dedups by response key and Sub returns one selection, so
// what an interceptor sees is the merged tree, and a walk over it is linear in
// that tree. This pins the property, because the cheap change to Fields -- drop
// the dedup and yield every possible type's copy -- would make it false and
// break no other test.
func TestSelectionWalkIsLinearInTheMergedTree(t *testing.T) {
	// Four interfaces, each with four implementations, nested four deep. The
	// DAG has far fewer nodes than the tree it unfolds to; a walk that saw
	// possible types separately would visit 4^4 leaves per level.
	const width, depth = 4, 4
	var sdl strings.Builder
	sdl.WriteString("type Query { root: N0! }\n")
	for d := range depth {
		fmt.Fprintf(&sdl, "interface N%d { id: ID!", d)
		if d+1 < depth {
			fmt.Fprintf(&sdl, " next: N%d", d+1)
		}
		sdl.WriteString(" }\n")
		for w := range width {
			fmt.Fprintf(&sdl, "type T%d_%d implements N%d { id: ID!", d, w, d)
			if d+1 < depth {
				fmt.Fprintf(&sdl, " next: N%d", d+1)
			}
			sdl.WriteString(" }\n")
		}
	}

	var q strings.Builder
	q.WriteString("{ root { id")
	for d := 1; d < depth; d++ {
		q.WriteString(" next { id")
	}
	for d := 1; d < depth; d++ {
		q.WriteString(" }")
	}
	q.WriteString(" } }")

	var visits int
	var seenSel func(Selection, int)
	seenSel = func(s Selection, level int) {
		if level > depth+2 {
			t.Fatalf("walk did not terminate by level %d", level)
		}
		for f := range s.Fields() {
			visits++
			if sub, ok := s.Sub(f.Name); ok {
				seenSel(sub, level+1)
			}
		}
	}

	opts := []SchemaOption{
		Query(Resolve("root", func(context.Context, Root) (any, error) { return nil, nil })),
	}
	for d := range depth {
		opts = append(opts, Interface[any](fmt.Sprintf("N%d", d)))
	}
	for d := range depth {
		for w := range width {
			name := fmt.Sprintf("T%d_%d", d, w)
			opts = append(opts, Object[struct{ ID ID }](name,
				Field("id", func(v *struct{ ID ID }) ID { return v.ID }),
			))
		}
	}
	// The object types declare next: N(d+1) in SDL; binding it as a resolver
	// keeps the shapes valid without needing real values.
	for d := 0; d+1 < depth; d++ {
		for w := range width {
			opts = append(opts, Object[struct{ ID ID }](fmt.Sprintf("T%d_%d", d, w),
				Resolve("next", func(context.Context, *struct{ ID ID }) (any, error) { return nil, nil }),
			))
		}
	}

	s, err := NewSchema(SDL(sdl.String()), opts...)
	if err != nil {
		t.Skipf("schema shape not expressible here: %v", err)
	}

	var captured Selection
	e := NewExecutor(s, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			if captured.IsEmpty() {
				captured = fc.Selection()
			}
			return next(ctx)
		})))
	run(t, e, q.String(), "")

	if captured.IsEmpty() {
		t.Fatal("the interceptor captured no selection")
	}
	seenSel(captured, 0)

	// One field per response key per level: root, then id+next at each level.
	// Anything near width^depth means Fields stopped merging possible types.
	const ceiling = 4 * depth
	if visits > ceiling {
		t.Errorf("a naive walk made %d visits over a %d-deep, %d-wide abstract plan; "+
			"Selection is exposing the expansion DAG, and an interceptor cannot memoize "+
			"on it because *selectionSet is unexported", visits, depth, width)
	}
	t.Logf("visits=%d for depth=%d width=%d (ceiling %d)", visits, depth, width, ceiling)
}
