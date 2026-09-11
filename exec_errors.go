package graphql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/vektah/gqlparser/v2/ast"
)

// pathNode is a reverse-linked path segment allocated once per composite
// value; leaf paths are materialized only when an error is recorded.
type pathNode struct {
	parent  *pathNode
	key     string
	index   int
	isIndex bool
}

func (n *pathNode) materialize() Path {
	depth := 0
	for p := n; p != nil; p = p.parent {
		depth++
	}
	if depth == 0 {
		return nil
	}
	out := make(Path, depth)
	for p := n; p != nil; p = p.parent {
		depth--
		if p.isIndex {
			out[depth] = PathElem{Index: p.index, IsIndex: true}
		} else {
			out[depth] = PathElem{Key: p.key}
		}
	}
	return out
}

// execState is the per-request execution state shared by all goroutines
// working on one operation.
type execState struct {
	e    *Executor
	s    *Schema
	vars map[string]any

	mu        sync.Mutex
	errs      []*Error
	cancelled atomic.Bool
}

// elementErrors reports errors for nullable list elements that were written
// as null while the rest of the list succeeded.
type elementErrors struct {
	errs []*indexedError
}

func (e *elementErrors) Error() string { return fmt.Sprintf("%d list element error(s)", len(e.errs)) }

func (e *elementErrors) add(i int, err error) *elementErrors {
	if e == nil {
		e = &elementErrors{}
	}
	e.errs = append(e.errs, &indexedError{i, err})
	return e
}

// addError presents err and appends it with the given path and location.
func (st *execState) addError(ctx context.Context, err error, path Path, pos *ast.Position) {
	presented := st.e.presenter(ctx, err)
	if presented == nil {
		return
	}
	if presented.Path == nil {
		presented.Path = path
	}
	if pos != nil && len(presented.Locations) == 0 {
		presented.Locations = []Location{{Line: pos.Line, Column: pos.Column}}
	}
	st.mu.Lock()
	st.errs = append(st.errs, presented)
	st.mu.Unlock()
}

// fieldError records an error raised while producing the value of f. List
// indices carried by indexedError extend the path; errNonNull becomes the
// specification's non-null violation message.
func (st *execState) fieldError(ctx context.Context, err error, path *pathNode, f *planField) {
	var soft *elementErrors
	if errors.As(err, &soft) {
		for _, ie := range soft.errs {
			st.fieldError(ctx, ie, path, f)
		}
		return
	}
	full := path
	if f != nil {
		full = &pathNode{parent: path, key: f.alias}
	}
	for {
		var ie *indexedError
		if !errors.As(err, &ie) {
			break
		}
		full = &pathNode{parent: full, index: ie.index, isIndex: true}
		err = ie.err
	}
	if errors.Is(err, errNonNull) {
		coord := "field"
		if f != nil && f.def != nil {
			coord = coordinate(f.def.object.name, f.def.name)
		}
		err = Errorf("Cannot return null for non-nullable field %s.", coord)
	}
	var pos *ast.Position
	if f != nil && f.ast != nil {
		pos = f.ast.Position
	}
	st.addError(ctx, err, full.materialize(), pos)
}

// nonNullError records a null value in a non-null position that was
// detected by the executor rather than by a writer.
func (st *execState) nonNullError(ctx context.Context, path *pathNode, f *planField) {
	st.fieldError(ctx, errNonNull, path, f)
}

// recovered converts a panic into a field error and logs the stack.
func (st *execState) recovered(ctx context.Context, r any, path *pathNode, f *planField) error {
	slog.ErrorContext(ctx, "graphql: resolver panic",
		"path", (&pathNode{parent: path, key: f.alias}).materialize().String(),
		"panic", r,
		"stack", string(debug.Stack()),
	)
	return &panicError{value: r}
}

// panicError is the error presented for a recovered panic. Its message is
// fixed so that internal details never leak to clients.
type panicError struct {
	value any
}

func (p *panicError) Error() string { return "internal system error" }

func (p *panicError) GraphQLExtensions() map[string]any {
	return map[string]any{"code": CodeInternal}
}
