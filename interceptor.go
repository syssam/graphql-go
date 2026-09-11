package graphql

import (
	"context"
	"fmt"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// RequestHandler executes a request; interceptors call it to continue.
type RequestHandler func(ctx context.Context, req *Request) *Response

// OperationHandler executes a compiled operation.
type OperationHandler func(ctx context.Context, oc *OperationContext) *Response

// FieldHandler resolves a field.
type FieldHandler func(ctx context.Context) (any, error)

// RequestInterceptor wraps the whole request, before parsing. Use it for
// authentication, logging, persisted query lookups and response extensions.
type RequestInterceptor interface {
	InterceptRequest(ctx context.Context, req *Request, next RequestHandler) *Response
}

// OperationInterceptor wraps execution of a parsed and planned operation.
// Use it for complexity limits, tracing spans and metrics.
type OperationInterceptor interface {
	InterceptOperation(ctx context.Context, oc *OperationContext, next OperationHandler) *Response
}

// FieldInterceptor wraps every field executor in the plan. It routes field
// results through any, so enable it only when field-level observation is
// worth the cost.
type FieldInterceptor interface {
	InterceptField(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error)
}

// RequestInterceptorFunc adapts a function to RequestInterceptor.
type RequestInterceptorFunc func(ctx context.Context, req *Request, next RequestHandler) *Response

func (f RequestInterceptorFunc) InterceptRequest(ctx context.Context, req *Request, next RequestHandler) *Response {
	return f(ctx, req, next)
}

// OperationInterceptorFunc adapts a function to OperationInterceptor.
type OperationInterceptorFunc func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response

func (f OperationInterceptorFunc) InterceptOperation(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
	return f(ctx, oc, next)
}

// FieldInterceptorFunc adapts a function to FieldInterceptor.
type FieldInterceptorFunc func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error)

func (f FieldInterceptorFunc) InterceptField(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
	return f(ctx, fc, next)
}

// WithInterceptors registers interceptors. Each value must implement at
// least one of RequestInterceptor, OperationInterceptor or FieldInterceptor;
// a value implementing several is registered for each. The first
// interceptor is the outermost.
func WithInterceptors(interceptors ...any) ExecutorOption {
	return func(e *Executor) {
		for _, i := range interceptors {
			matched := false
			if ri, ok := i.(RequestInterceptor); ok {
				e.reqInterceptors = append(e.reqInterceptors, ri)
				matched = true
			}
			if oi, ok := i.(OperationInterceptor); ok {
				e.opInterceptors = append(e.opInterceptors, oi)
				matched = true
			}
			if fi, ok := i.(FieldInterceptor); ok {
				e.fieldInterceptors = append(e.fieldInterceptors, fi)
				matched = true
			}
			if !matched {
				panic(fmt.Sprintf("graphql: %T implements no interceptor interface", i))
			}
		}
	}
}

func (e *Executor) buildChains() {
	e.reqChain = e.execute
	for i := len(e.reqInterceptors) - 1; i >= 0; i-- {
		next, ri := e.reqChain, e.reqInterceptors[i]
		e.reqChain = func(ctx context.Context, req *Request) *Response {
			return ri.InterceptRequest(ctx, req, next)
		}
	}
	e.opChain = e.runOperation
	for i := len(e.opInterceptors) - 1; i >= 0; i-- {
		next, oi := e.opChain, e.opInterceptors[i]
		e.opChain = func(ctx context.Context, oc *OperationContext) *Response {
			return oi.InterceptOperation(ctx, oc, next)
		}
	}
}

// interceptedExec wraps a plan field's executor with the executor's field
// interceptors. Leaf results travel through any and are written with the
// field's type-erased writer.
func (e *Executor) interceptedExec(pf *planField) fieldExec {
	fd := pf.def
	inner := fd.anyResolve
	chain := func(ctx context.Context, parent, args any) (any, error) {
		fc := FieldFrom(ctx)
		if fc == nil {
			fc = &FieldContext{Field: fd.def, Object: fd.object.def, Args: args, Parent: parent, field: pf}
			ctx = withField(ctx, fc)
		}
		handler := FieldHandler(func(ctx context.Context) (any, error) { return inner(ctx, parent, args) })
		for i := len(e.fieldInterceptors) - 1; i >= 0; i-- {
			next, fi := handler, e.fieldInterceptors[i]
			handler = func(ctx context.Context) (any, error) { return fi.InterceptField(ctx, fc, next) }
		}
		return handler(ctx)
	}
	if fd.leaf {
		writeAny, typ := fd.writeAny, fd.typ
		return fieldExec{writeLeaf: func(ctx context.Context, w *jsonw.Writer, parent, args any) error {
			v, err := chain(ctx, parent, args)
			if err != nil {
				return err
			}
			return writeAny(w, v, typ)
		}}
	}
	return fieldExec{resolve: chain}
}
