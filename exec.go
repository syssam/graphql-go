package graphql

import (
	"context"
	"runtime"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/rules"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Executor runs operations against a Schema. It owns the plan cache and the
// concurrency budget and is safe for concurrent use.
type Executor struct {
	schema         *Schema
	cache          *planCache
	sem            chan struct{}
	maxConcurrency int
	presenter      ErrorPresenter
	recover        bool
	rules          *rules.Rules

	reqInterceptors   []RequestInterceptor
	opInterceptors    []OperationInterceptor
	fieldInterceptors []FieldInterceptor
	reqChain          RequestHandler
	opChain           OperationHandler
}

// ExecutorOption configures an Executor.
type ExecutorOption func(*Executor)

// WithMaxConcurrency bounds the number of resolver goroutines running at
// once across all requests. Fields that cannot obtain a slot run inline.
// The default is 4 × GOMAXPROCS; zero disables concurrency.
func WithMaxConcurrency(n int) ExecutorOption {
	return func(e *Executor) { e.maxConcurrency = n }
}

// WithPlanCache sets the number of parsed documents kept in the LRU cache.
// Zero disables caching. The default is 1024.
func WithPlanCache(size int) ExecutorOption {
	return func(e *Executor) { e.cache = newPlanCache(size) }
}

// WithErrorPresenter replaces DefaultErrorPresenter.
func WithErrorPresenter(p ErrorPresenter) ExecutorOption {
	return func(e *Executor) { e.presenter = p }
}

// WithRecover controls whether resolver panics are converted into
// INTERNAL_SERVER_ERROR field errors. It is enabled by default; disable it
// only in tests that want panics to surface.
func WithRecover(enabled bool) ExecutorOption {
	return func(e *Executor) { e.recover = enabled }
}

// NewExecutor creates an executor for s.
func NewExecutor(s *Schema, opts ...ExecutorOption) *Executor {
	e := &Executor{
		schema:         s,
		maxConcurrency: runtime.GOMAXPROCS(0) * 4,
		presenter:      DefaultErrorPresenter,
		recover:        true,
	}
	for _, o := range opts {
		o(e)
	}
	if e.cache == nil {
		e.cache = newPlanCache(1024)
	}
	if e.maxConcurrency > 0 {
		e.sem = make(chan struct{}, e.maxConcurrency)
	}
	e.rules = rules.NewDefaultRules()
	if !s.introspection {
		e.rules.AddRule(noIntrospectionRule.Name, noIntrospectionRule.RuleFunc)
	}
	e.buildChains()
	return e
}

// Schema returns the executor's schema.
func (e *Executor) Schema() *Schema { return e.schema }

// Execute runs a query or mutation and returns its response. Call
// Response.Release once the response has been serialized.
func (e *Executor) Execute(ctx context.Context, req *Request) *Response {
	return e.reqChain(ctx, req)
}

// OperationKind reports the kind of the operation a request would execute,
// using the document cache. Transports use it to reject mutations over GET.
func (e *Executor) OperationKind(query, operationName string) (ast.Operation, error) {
	entry, errs := e.document(query)
	if errs != nil {
		return "", errs[0]
	}
	op, err := selectOperation(entry.doc, operationName)
	if err != nil {
		return "", err
	}
	return op.Operation, nil
}

// document returns the parsed and validated document for query.
func (e *Executor) document(query string) (*docEntry, []*Error) {
	if entry := e.cache.get(query); entry != nil {
		return entry, nil
	}
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return nil, gqlErrors(err, CodeParseFailed)
	}
	if verrs := validator.ValidateWithRules(e.schema.ast, doc, e.rules); len(verrs) > 0 {
		return nil, gqlErrors(verrs, CodeValidationFailed)
	}
	entry := &docEntry{query: query, doc: doc, condVars: condVariables(doc)}
	e.cache.put(entry)
	return entry, nil
}

func gqlErrors(err error, code string) []*Error {
	var out []*Error
	switch v := err.(type) {
	case gqlerror.List:
		for _, ge := range v {
			out = append(out, fromGQLError(ge).WithCode(code))
		}
	case *gqlerror.Error:
		out = append(out, fromGQLError(v).WithCode(code))
	default:
		out = append(out, Errorf("%v", err).WithCode(code))
	}
	return out
}

func selectOperation(doc *ast.QueryDocument, name string) (*ast.OperationDefinition, *Error) {
	if name == "" {
		if len(doc.Operations) != 1 {
			return nil, Errorf("Must provide operation name if query contains multiple operations.").WithCode(CodeOperationResolution)
		}
		return doc.Operations[0], nil
	}
	op := doc.Operations.ForName(name)
	if op == nil {
		return nil, Errorf("Unknown operation named %q.", name).WithCode(CodeOperationResolution)
	}
	return op, nil
}

// execute is the innermost request handler: parse, plan, coerce variables
// and hand over to the operation chain.
func (e *Executor) execute(ctx context.Context, req *Request) *Response {
	start := time.Now()
	entry, errs := e.document(req.Query)
	if errs != nil {
		return e.requestError(ctx, errs...)
	}
	op, oerr := selectOperation(entry.doc, req.OperationName)
	if oerr != nil {
		return e.requestError(ctx, oerr)
	}
	if op.Operation == ast.Subscription {
		return e.requestError(ctx, Errorf("Subscriptions must be executed through Executor.Subscribe.").WithCode(CodeOperationResolution))
	}

	rawVars, err := decodeVariables(req.Variables)
	if err != nil {
		return e.requestError(ctx, Errorf("%v", err).WithCode(CodeBadUserInput))
	}
	vars, verr := e.schema.coerceVariables(op, rawVars)
	if verr != nil {
		return e.requestError(ctx, verr)
	}

	cacheHit := entry.plans != nil
	p, perrs := entry.planFor(e.schema, e, op, vars)
	if perrs != nil {
		return e.requestError(ctx, perrs...)
	}

	oc := &OperationContext{
		Operation:     op,
		Doc:           entry.doc,
		RawQuery:      req.Query,
		OperationName: req.OperationName,
		Variables:     vars,
		Stats:         OperationStats{Start: start, CacheHit: cacheHit},
		plan:          p,
		entry:         entry,
	}
	return e.opChain(withOperation(ctx, oc), oc)
}

// requestError builds a response for errors raised before execution.
func (e *Executor) requestError(ctx context.Context, errs ...*Error) *Response {
	resp := &Response{Errors: make([]*Error, 0, len(errs))}
	for _, err := range errs {
		resp.Errors = append(resp.Errors, e.presenter(ctx, err))
	}
	return resp
}

// runOperation executes a planned operation and produces the response.
func (e *Executor) runOperation(ctx context.Context, oc *OperationContext) *Response {
	p := oc.plan
	w := jsonw.Get()
	st := &execState{e: e, s: e.schema, vars: oc.Variables}
	ok := st.writeObject(ctx, w, p.root, p.sel, &Root{}, nil, p.op.Operation == ast.Mutation)
	if !ok {
		w.Reset()
		w.Null()
	}
	return &Response{Data: w.Bytes(), Errors: st.errs, buf: w}
}
