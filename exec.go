package graphql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/syssam/graphql-go/internal/jsonw"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/core"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

// Executor runs operations against a Schema. It owns the plan cache and the
// concurrency budget and is safe for concurrent use.
type Executor struct {
	schema         *Schema
	cache          *planCache
	cacheSize      int
	cacheBytes     int64
	sem            chan struct{}
	maxConcurrency int
	presenter      ErrorPresenter
	recover        bool
	rules          *rules.Rules
	noSuggestions  bool

	reqInterceptors   []RequestInterceptor
	opInterceptors    []OperationInterceptor
	fieldInterceptors []FieldInterceptor
	fieldObservers    []FieldObserver
	reqChain          RequestHandler
	opChain           OperationHandler

	maxComplexity int
	maxDepth      int
	cost          *QueryCost
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
	return func(e *Executor) { e.cacheSize = size }
}

// WithPlanCacheBytes bounds the query text the plan cache holds, evicting the
// least recently used documents to stay within it. Zero means no byte limit;
// the entry count from WithPlanCache still applies. The default is 16 MiB.
//
// The budget is on query text because that is what can be counted cheaply, but
// what it actually bounds is memory: a parsed and validated document retained
// roughly 26 times its query text when measured on an alias-heavy query (other
// shapes will differ), so without this limit 1024 distinct valid 1 MiB queries
// of that shape hold tens of gigabytes. Ordinary queries are a few kilobytes,
// so the default entry count binds long before this does and only unusually
// large queries ever reach it. A query larger than the whole budget is not
// cached but still executes.
func WithPlanCacheBytes(n int64) ExecutorOption {
	return func(e *Executor) { e.cacheBytes = n }
}

// WithErrorPresenter replaces DefaultErrorPresenter.
func WithErrorPresenter(p ErrorPresenter) ExecutorOption {
	return func(e *Executor) { e.presenter = p }
}

// suggestionFree pairs each gqlparser rule that volunteers a "Did you mean"
// clause with the variant that does not. gqlparser ships five; gqlgen swaps
// two, leaving argument names, type names and input field names disclosed.
var suggestionFree = []struct {
	suggesting string
	quiet      core.Rule
}{
	{"FieldsOnCorrectType", rules.FieldsOnCorrectTypeRuleWithoutSuggestions},
	{"KnownArgumentNames", rules.KnownArgumentNamesRuleWithoutSuggestions},
	{"KnownTypeNames", rules.KnownTypeNamesRuleWithoutSuggestions},
	{"ScalarLeafs", rules.ScalarLeafsRuleWithoutSuggestions},
	{"ValuesOfCorrectType", rules.ValuesOfCorrectTypeRuleWithoutSuggestions},
}

// DisableSuggestions withholds the "Did you mean" clause from validation
// errors. DisableIntrospection alone does not close that channel: the
// suggestions name neighbouring fields, types, arguments and input fields,
// so a schema stays enumerable one typo at a time. The errors still report
// what was wrong, only without naming what would have been right.
func DisableSuggestions() ExecutorOption {
	return func(e *Executor) { e.noSuggestions = true }
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
		cacheSize:      1024,
		cacheBytes:     16 << 20,
	}
	for _, o := range opts {
		o(e)
	}
	// Built after every option has run, so WithPlanCache and WithPlanCacheBytes
	// compose in either order.
	e.cache = newPlanCache(e.cacheSize, e.cacheBytes)
	if e.maxConcurrency > 0 {
		e.sem = make(chan struct{}, e.maxConcurrency)
	}
	e.rules = rules.NewDefaultRules()
	if e.noSuggestions {
		for _, r := range suggestionFree {
			e.rules.RemoveRule(r.suggesting)
			e.rules.AddRule(r.quiet.Name, r.quiet.RuleFunc)
		}
	}
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
	var list gqlerror.List
	var gerr *gqlerror.Error
	switch {
	case errors.As(err, &list):
		for _, ge := range list {
			out = append(out, fromGQLError(ge).WithCode(code))
		}
	case errors.As(err, &gerr):
		out = append(out, fromGQLError(gerr).WithCode(code))
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
		return e.requestError(ctx, Errorf("Subscription operations must be run with Subscribe over a streaming transport, not Execute.").WithCode(CodeOperationResolution))
	}

	rawVars, err := decodeVariables(req.Variables)
	if err != nil {
		return e.requestError(ctx, Errorf("%v", err).WithCode(CodeBadUserInput))
	}
	vars, verr := e.schema.coerceVariables(op, rawVars)
	if verr != nil {
		return e.requestError(ctx, verr)
	}

	p, cacheHit, perrs := entry.planFor(e.schema, e, op, vars)
	if perrs != nil {
		return e.requestError(ctx, perrs...)
	}

	oc := &OperationContext{
		Operation:     op,
		Doc:           entry.doc,
		RawQuery:      req.Query,
		OperationName: req.OperationName,
		Variables:     vars,
		Stats:         OperationStats{Start: start, CacheHit: cacheHit, PlanUncacheable: entry.planUncacheable()},
		plan:          p,
		entry:         entry,
		hub:           newWaveCoordinator(),
	}
	// The cost is computed here rather than at the limit check because a
	// rate limiter is an operation interceptor, and those wrap that check
	// rather than following it. Without this oc.Cost() hands every
	// interceptor the no-model fallback even when a model is configured.
	if e.cost != nil {
		oc.ensureCost(*e.cost)
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

// runOperation executes a planned operation and produces the response. It is
// the bottom of the operation chain for one query or mutation, and for one
// event of a subscription.
func (e *Executor) runOperation(ctx context.Context, oc *OperationContext) *Response {
	if oc.event != nil {
		return e.runSubscriptionEvent(ctx, oc)
	}
	p := oc.plan
	w := jsonw.Get()
	st := &execState{e: e, s: e.schema, vars: oc.Variables}
	ok := st.writeObject(ctx, w, p.root, p.sel, &Root{}, nil, p.op.Operation == ast.Mutation)
	if !ok {
		w.Reset()
		w.Null()
	}
	st.reportActualCost(e, oc)
	return e.finishResponse(oc, w, st)
}

// reportActualCost publishes what execution measured. A rejected or
// never-executed operation leaves actualOK false rather than reporting zero.
func (st *execState) reportActualCost(e *Executor, oc *OperationContext) {
	if e.cost == nil || !e.cost.Actual {
		return
	}
	oc.actualCost = st.actualCost.Load()
	oc.actualOK = true
}

// finishResponse packages a written buffer and the collected errors, taking
// over ownership of w so the caller must not touch it again.
func (e *Executor) finishResponse(oc *OperationContext, w *jsonw.Writer, st *execState) *Response {
	resp := &Response{Data: w.Bytes(), Errors: st.errs, buf: w}
	oc.mu.Lock()
	if len(oc.extensions) > 0 {
		resp.Extensions = maps.Clone(oc.extensions)
	}
	oc.mu.Unlock()
	return resp
}

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

	mu   sync.Mutex
	errs []*Error

	// actualCost accumulates the weight of every field resolved. Sibling
	// resolvers run concurrently, so it is atomic; it is only ever touched
	// for fields whose costWeight is non-zero, which no field has unless
	// actual cost is enabled.
	//
	// It is 32 bits, and sits next to cancelled, so that the two share the
	// padding the struct already had: as an int64 it pushed execState into
	// the next size class and cost 16 bytes on every request, including the
	// requests of everyone who never turns cost accounting on. Overflow needs
	// two billion resolved fields in one operation, which the complexity and
	// cost caps exist to prevent and which would take minutes to reach.
	actualCost atomic.Int32
	cancelled  atomic.Bool
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
