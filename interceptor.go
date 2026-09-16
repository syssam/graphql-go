package graphql

import (
	"context"

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
// The shape follows gRPC's UnaryServerInterceptor: observe, call next, observe.
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

// InterceptRequest implements RequestInterceptor.
func (f RequestInterceptorFunc) InterceptRequest(ctx context.Context, req *Request, next RequestHandler) *Response {
	return f(ctx, req, next)
}

// OperationInterceptorFunc adapts a function to OperationInterceptor.
type OperationInterceptorFunc func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response

// InterceptOperation implements OperationInterceptor.
func (f OperationInterceptorFunc) InterceptOperation(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
	return f(ctx, oc, next)
}

// FieldInterceptorFunc adapts a function to FieldInterceptor.
type FieldInterceptorFunc func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error)

// InterceptField implements FieldInterceptor.
func (f FieldInterceptorFunc) InterceptField(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
	return f(ctx, fc, next)
}

// WithRequestInterceptor registers request interceptors. The first is the
// outermost, matching grpc.ChainUnaryInterceptor.
func WithRequestInterceptor(is ...RequestInterceptor) ExecutorOption {
	return func(e *Executor) { e.reqInterceptors = append(e.reqInterceptors, is...) }
}

// WithOperationInterceptor registers operation interceptors. The first is
// the outermost.
func WithOperationInterceptor(is ...OperationInterceptor) ExecutorOption {
	return func(e *Executor) { e.opInterceptors = append(e.opInterceptors, is...) }
}

// WithFieldInterceptor registers field interceptors. The first is the
// outermost. Field interceptors force every field through the type-erased
// path; register them only when observation is worth that cost.
func WithFieldInterceptor(is ...FieldInterceptor) ExecutorOption {
	return func(e *Executor) { e.fieldInterceptors = append(e.fieldInterceptors, is...) }
}

// ChainRequestInterceptors composes interceptors so that the first is
// outermost. A nil or empty list is a no-op interceptor.
func ChainRequestInterceptors(is ...RequestInterceptor) RequestInterceptor {
	switch len(is) {
	case 0:
		return RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
			return next(ctx, req)
		})
	case 1:
		return is[0]
	}
	return RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
		h := next
		for i := len(is) - 1; i >= 0; i-- {
			inner, ri := h, is[i]
			h = func(ctx context.Context, req *Request) *Response { return ri.InterceptRequest(ctx, req, inner) }
		}
		return h(ctx, req)
	})
}

// ChainOperationInterceptors composes interceptors so that the first is outermost.
func ChainOperationInterceptors(is ...OperationInterceptor) OperationInterceptor {
	switch len(is) {
	case 0:
		return OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			return next(ctx, oc)
		})
	case 1:
		return is[0]
	}
	return OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
		h := next
		for i := len(is) - 1; i >= 0; i-- {
			inner, oi := h, is[i]
			h = func(ctx context.Context, oc *OperationContext) *Response {
				return oi.InterceptOperation(ctx, oc, inner)
			}
		}
		return h(ctx, oc)
	})
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
	if e.maxComplexity > 0 || e.maxDepth > 0 || e.cost != nil {
		next := e.opChain
		e.opChain = func(ctx context.Context, oc *OperationContext) *Response {
			if err := e.rejectIfOverLimit(oc); err != nil {
				resp := e.requestError(ctx, err)
				e.attachCost(oc, resp)
				return resp
			}
			resp := next(ctx, oc)
			e.attachCost(oc, resp)
			return resp
		}
	}
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
	chain := func(ctx context.Context, parent, args any, fc *FieldContext) (any, error) {
		handler := FieldHandler(func(ctx context.Context) (any, error) { return inner(ctx, parent, args) })
		for i := len(e.fieldInterceptors) - 1; i >= 0; i-- {
			next, fi := handler, e.fieldInterceptors[i]
			handler = func(ctx context.Context) (any, error) { return fi.InterceptField(ctx, fc, next) }
		}
		return handler(ctx)
	}
	if fd.leaf {
		writeAny, typ := fd.writeAny, fd.typ
		return fieldExec{writeLeaf: func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error {
			v, err := chain(ctx, parent, args, fc)
			if err != nil {
				return err
			}
			return writeAny(w, v, typ)
		}}
	}
	return fieldExec{resolve: chain}
}
