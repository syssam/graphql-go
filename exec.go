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
	schema     *Schema
	cache      *planCache
	cacheSize  int
	cacheBytes int64

	// docMu and docCalls let concurrent misses for the same query text share
	// one parse and validation instead of each doing it.
	docMu          sync.Mutex
	docCalls       map[string]*docCall
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
	subInterceptors   []SubscriptionInterceptor
	reqChain          RequestHandler
	opChain           OperationHandler

	maxComplexity    int
	maxDepth         int
	maxResponseBytes int64
	maxErrors        int

	// planHits and planMisses count operations served from, or compiled
	// into, the plan cache. They live on the Executor, not per request.
	planHits         atomic.Int64
	planMisses       atomic.Int64
	operationTimeout time.Duration
	// timeoutCause is the context cause of this executor's own deadline, nil
	// without one. Compared by identity, it tells the timeout apart from a
	// deadline the caller set.
	timeoutCause error
	cost         *QueryCost
	authorizer   Authorizer

	objectAuthorizer ObjectAuthorizer
	objectAuthBatch  int
}

// ExecutorOption configures an Executor.
type ExecutorOption func(*Executor)

// WithMaxConcurrency sets the slot budget shared by every request's
// concurrently scheduled fields, which Stats reports as ConcurrencyInUse and
// ConcurrencyLimit. It does not cap goroutines: each concurrently scheduled
// field or list element runs on its own goroutine and takes a slot only if
// one is free, because a field waiting for a slot would hold back the
// DataLoader wave its siblings are parked in. What bounds the work of one
// request is its size, so bound that with WithMaxComplexity, WithQueryCost
// and WithMaxResponseBytes. Zero disables concurrent resolution: every field
// runs inline, in order. The default is 4 × GOMAXPROCS.
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
	return func(e *Executor) {
		// The engine writes Path, Locations and the ordering key onto what the
		// presenter returns. DefaultErrorPresenter always returns a copy; a
		// custom one may return the error it was given or a package-level
		// masked error, which those writes would race on across requests and
		// leak one request's path into another's response. A shallow copy is
		// enough: the engine assigns those fields and never appends to them.
		e.presenter = func(ctx context.Context, err error) *Error {
			out := p(ctx, err)
			if out == nil {
				return nil
			}
			c := *out
			return &c
		}
	}
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
// INTERNAL_SERVER_ERROR field errors, and Authorizer panics into the same
// error for the whole operation or event. A subscription event's interceptors
// and presenter run on the executor's own goroutine rather than the caller's,
// so a panic there becomes an error event instead of reaching no recover at
// all. It is enabled by default; disable it only in tests that want panics to
// surface.
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

		maxResponseBytes: 64 << 20,
		maxErrors:        1000,

		objectAuthBatch: 50,
	}
	for _, o := range opts {
		o(e)
	}
	// Built after every option has run, so WithPlanCache and WithPlanCacheBytes
	// compose in either order.
	e.cache = newPlanCache(e.cacheSize, e.cacheBytes)
	if e.operationTimeout > 0 {
		e.timeoutCause = &timeoutError{d: e.operationTimeout}
	}
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
	if e.timeoutCause != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, e.operationTimeout, e.timeoutCause)
		defer cancel()
	}
	return e.reqChain(ctx, req)
}

// timeoutError is the cause attached to the executor's own deadline.
type timeoutError struct{ d time.Duration }

func (t *timeoutError) Error() string {
	return fmt.Sprintf("operation exceeded its timeout of %v", t.d)
}

// Is makes the cause match context.DeadlineExceeded, as ctx.Err() does, so a
// resolver returning context.Cause(ctx) is recognised as timed out too.
func (t *timeoutError) Is(target error) bool { return target == context.DeadlineExceeded }

// timeoutFieldError reports err as the operation timeout without losing what
// the resolver said: err stays in the chain for a presenter, and an *Error's
// extensions are kept beside the code.
func (e *Executor) timeoutFieldError(err error) *Error {
	out := &Error{Message: e.timeoutCause.Error(), Err: err}
	var inner *Error
	if errors.As(err, &inner) {
		for k, v := range inner.Extensions {
			out = out.WithExtension(k, v)
		}
	}
	return out.WithCode(CodeOperationTimeout)
}

// timedOut reports whether ctx ended because of this executor's timeout
// rather than anything the caller did.
func (e *Executor) timedOut(ctx context.Context) bool {
	// Identity, not errors.Is: the question is whether this executor's own
	// cause value ended the context. errors.Is would also match a resolver
	// that wrapped and returned that value, reporting OPERATION_TIMEOUT for a
	// deadline the caller owns -- the split this function exists to make.
	//nolint:errorlint // deliberate identity comparison; see above
	return e.timeoutCause != nil && context.Cause(ctx) == e.timeoutCause
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

// testHookParseDocument runs just before a document is parsed. Tests use it
// to hold a parse open while other requests for the same text arrive.
var testHookParseDocument func()

// document returns the parsed and validated document for query. Concurrent
// misses for the same text share one parse: parsing and validating a 100 KB
// query measured about 8 ms and 6.8 MB, and a cold cache met by 64 identical
// requests at once took 19 times as long as one. Only a success is cached; a
// failure is shared with the requests already waiting on it and no further,
// so distinct invalid queries cannot push valid documents out of the cache.
func (e *Executor) document(query string) (*docEntry, []*Error) {
	if entry := e.cache.get(query); entry != nil {
		return entry, nil
	}
	e.docMu.Lock()
	if call, ok := e.docCalls[query]; ok {
		e.docMu.Unlock()
		<-call.done
		return call.shared()
	}
	// The request that held this text may have cached it and left between
	// the lookup above and taking the lock.
	if entry := e.cache.get(query); entry != nil {
		e.docMu.Unlock()
		return entry, nil
	}
	call := &docCall{done: make(chan struct{})}
	if e.docCalls == nil {
		e.docCalls = make(map[string]*docCall)
	}
	e.docCalls[query] = call
	e.docMu.Unlock()

	// Deferred so a panic while parsing still releases the waiters, who then
	// see a call with neither an entry nor errors.
	defer func() {
		e.docMu.Lock()
		delete(e.docCalls, query)
		e.docMu.Unlock()
		close(call.done)
	}()
	entry, errs := e.parseDocument(query)
	call.entry = entry
	// Waiters get copies of copies: the leader presents errs itself, and a
	// presenter that annotates its *Error would otherwise be writing while a
	// waiter reads the same object.
	if errs != nil {
		call.errs = make([]*Error, len(errs))
		for i, err := range errs {
			call.errs[i] = err.clone()
		}
	}
	return entry, errs
}

// docCall is one in-progress parse of a query text.
type docCall struct {
	done  chan struct{}
	entry *docEntry
	errs  []*Error
}

// shared returns the call's result to a request that waited on it. Errors are
// copied because each request presents its own, and a presenter may annotate
// the *Error it is given.
func (c *docCall) shared() (*docEntry, []*Error) {
	if c.entry == nil && c.errs == nil {
		return nil, []*Error{Errorf("The query could not be parsed.").WithCode(CodeInternal)}
	}
	if c.errs == nil {
		return c.entry, nil
	}
	errs := make([]*Error, len(c.errs))
	for i, err := range c.errs {
		errs[i] = err.clone()
	}
	return nil, errs
}

// parseDocument parses, validates and caches query.
func (e *Executor) parseDocument(query string) (*docEntry, []*Error) {
	if testHookParseDocument != nil {
		testHookParseDocument()
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
	oc, errs := e.prepareOperation(req, false)
	if errs != nil {
		return e.requestError(ctx, errs...)
	}
	return e.opChain(withOperation(ctx, oc), oc)
}

// prepareOperation is everything execute and Subscribe share before they
// diverge: document, operation, variables, plan and cost. It is one function
// because two copies drifted: Subscribe once skipped the cost limit and left
// every event with the no-model cost.
func (e *Executor) prepareOperation(req *Request, subscription bool) (*OperationContext, []*Error) {
	start := time.Now()
	entry, errs := e.document(req.Query)
	if errs != nil {
		return nil, errs
	}
	op, oerr := selectOperation(entry.doc, req.OperationName)
	if oerr != nil {
		return nil, []*Error{oerr}
	}
	switch {
	case subscription && op.Operation != ast.Subscription:
		return nil, []*Error{Errorf("Subscribe requires a subscription operation, got %s.", op.Operation).WithCode(CodeOperationResolution)}
	case !subscription && op.Operation == ast.Subscription:
		return nil, []*Error{Errorf("Subscription operations must be run with Subscribe over a streaming transport, not Execute.").WithCode(CodeOperationResolution)}
	}

	rawVars, err := decodeVariables(req.Variables)
	if err != nil {
		return nil, []*Error{Errorf("%v", err).WithCode(CodeBadUserInput)}
	}
	vars, verr := e.schema.coerceVariables(op, rawVars)
	if verr != nil {
		return nil, []*Error{verr}
	}

	p, cacheHit, perrs := entry.planFor(e.schema, e, op, vars)
	if perrs != nil {
		return nil, perrs
	}
	e.countPlan(cacheHit)

	oc := &OperationContext{
		Operation:     op,
		Doc:           entry.doc,
		RawQuery:      req.Query,
		OperationName: req.OperationName,
		Variables:     vars,
		Stats:         OperationStats{Start: start, CacheHit: cacheHit, PlanUncacheable: entry.planUncacheable()},
		plan:          p,
		entry:         entry,
	}
	oc.startWaves()
	// The cost is computed here rather than at the limit check because a
	// rate limiter is an operation interceptor, and those wrap that check
	// rather than following it. Without this oc.Cost() hands every
	// interceptor the no-model fallback even when a model is configured.
	if e.cost != nil {
		oc.ensureCost(*e.cost)
	}
	return oc, nil
}

// requestError builds a response for errors raised before execution. Each
// err is presented exactly once here: a caller must pass a raw, unpresented
// *Error (Errorf(...).WithCode(...), or authorizerError of some other
// error), never the output of e.presenter itself, or a custom ErrorPresenter
// that logs or attaches an incident ID would run twice per rejection.
func (e *Executor) requestError(ctx context.Context, errs ...*Error) *Response {
	omitted := e.maxErrors > 0 && len(errs) > e.maxErrors
	if omitted {
		errs = errs[:e.maxErrors]
	}
	resp := &Response{Errors: make([]*Error, 0, len(errs)+1)}
	for _, err := range errs {
		p := e.presenter(ctx, err)
		if p == nil {
			// A presenter may drop a field error, but not this one: a
			// response without data has to carry an error saying why.
			p = Errorf("request refused")
			if code, ok := err.Extensions["code"]; ok {
				p = p.WithExtension("code", code)
			}
		}
		resp.Errors = append(resp.Errors, p)
	}
	if omitted {
		resp.Errors = append(resp.Errors, errorLimitNotice())
	}
	return resp
}

// errorLimitNotice stands in for the errors dropped past WithMaxErrors. It is
// not presented: it carries nothing a presenter could need to mask, and an
// unpresented code is what lets the executor find it again.
func errorLimitNotice() *Error {
	return Errorf("Too many errors: further errors were omitted.").WithCode(CodeErrorLimitExceeded)
}

// authorizerCause holds a non-*Error Authorize failure behind Error's Err
// field so a custom ErrorPresenter can still test the cause with errors.Is
// (errPolicyDown, for instance), without exposing it to errors.As. Deliberately
// no Unwrap: DefaultErrorPresenter (and any other presenter) walks the error
// chain with errors.As looking for an ExtensionsProvider, and if this type
// unwrapped to the policy backend's own error, whatever extensions that
// error's type carries -- an internal host, a trace ID -- would be merged
// into the client-visible response right along with it. Do not add Unwrap or
// As here; that is exactly the leak this type exists to close.
type authorizerCause struct {
	cause error
}

// Error deliberately does not return cause's text: it is what the message
// would be if something stringified Err directly instead of going through
// Error.Message, so it must carry no more than Message already does.
func (c *authorizerCause) Error() string { return "internal system error" }

// Is delegates to the real cause so errors.Is(presented, errPolicyDown)
// still works, even though errors.As cannot see past this wrapper.
func (c *authorizerCause) Is(target error) bool { return errors.Is(c.cause, target) }

// authorizerError prepares an Authorize failure for presentation. An *Error
// the Authorizer built on purpose passes through. Anything else is typically a
// policy backend's own failure -- a transport error carrying an internal
// address -- and would otherwise reach the client verbatim, both disclosing
// the address and making an outage look like a denial. It is presented as a
// generic internal error with the original cause logged in full and kept,
// behind authorizerCause, for a presenter that wants to test it -- unless it
// is a recovered panic, which runAuthorizer has already logged.
func authorizerError(ctx context.Context, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var p *panicError
	if !errors.As(err, &p) {
		slog.ErrorContext(ctx, "graphql: authorizer failed", "error", err)
	}
	return (&Error{Message: "internal system error", Err: &authorizerCause{cause: err}}).WithCode(CodeInternal)
}

// authorize runs the Authorizer against p's shape and returns the resulting
// Decision, or nil when no Authorizer is configured or the shape is empty —
// which is also what makes the call free for an operation that touches
// nothing requiring authorization. A non-nil error rejects the whole
// operation, one subscription event, or — when called as the stream opens —
// the subscription itself.
func (e *Executor) authorize(ctx context.Context, p *plan, vars map[string]any) (*Decision, error) {
	if e.authorizer == nil || p.shape.IsEmpty() {
		return nil, nil
	}
	return e.runAuthorizer(ctx, p, vars)
}

// runAuthorizer is split from authorize so the disabled path carries no
// deferred recover. The recover matters beyond tidiness: per-event
// authorization runs in pump's goroutine, where a panicking policy client
// would end the process rather than one request.
func (e *Executor) runAuthorizer(ctx context.Context, p *plan, vars map[string]any) (d *Decision, err error) {
	if e.recover {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "graphql: authorizer panic",
					"panic", r,
					"stack", string(debug.Stack()),
				)
				d, err = nil, &panicError{value: r}
			}
		}()
	}
	d = newDecision(p.shape)
	// The walk sits inside the recover above, so a panic in it is handled
	// like an Authorizer's. It is gated so a plan with no argument site walks
	// and allocates nothing.
	if p.shape.hasArgSites {
		src := &decisionSource{shape: p.shape, inputs: make([][]InputKey, len(p.shape.sites))}
		// By index: an AuthSite is 136 bytes and this runs per request, so
		// ranging by value memmoves the whole table every time.
		for i := range p.shape.sites {
			s := &p.shape.sites[i]
			if s.argValue != nil {
				src.inputs[i] = inputKeys(e.schema.ast.Types, s.argType, s.argValue, vars)
			}
		}
		d.src = src
	}
	if err := e.authorizer.Authorize(ctx, p.shape, d); err != nil {
		return nil, err
	}
	return d, nil
}

// runOperation executes a planned operation and produces the response. It is
// the bottom of the operation chain for one query or mutation, and for one
// event of a subscription. The Authorizer runs here, above the event branch,
// so a subscription gets a fresh Decision for every event: each event builds
// its own OperationContext and runs the whole operation chain, which is what
// lets a mid-stream scope revocation take effect on the very next event
// without tearing down the stream — an Authorize error rejects that one
// event's Response, and runSubscriptionEvent returns normally either way, so
// pump keeps draining the source.
func (e *Executor) runOperation(ctx context.Context, oc *OperationContext) *Response {
	p := oc.plan
	decision, err := e.authorize(ctx, p, oc.Variables)
	if err != nil {
		return e.requestError(ctx, authorizerError(ctx, err))
	}
	if decision != nil {
		// SelectedField.Withheld reads it, so code planning what a subtree
		// loads can skip one this request will not resolve.
		ctx = withDecision(ctx, oc, decision)
	}
	if oc.event != nil {
		return e.runSubscriptionEvent(ctx, oc, decision)
	}
	w := e.newResponseWriter()
	st := &execState{e: e, vars: oc.Variables, decision: decision}
	ok := st.writeObject(ctx, w, p.root, p.sel, &Root{}, nil, p.op.Operation == ast.Mutation)
	st.finishData(ctx, w, ok)
	st.reportActualCost(e, oc)
	return e.finishResponse(oc, w, st)
}

// newResponseWriter returns the root writer for one response, carrying the
// executor's size limit when one is set.
func (e *Executor) newResponseWriter() *jsonw.Writer {
	w := jsonw.Get()
	if e.maxResponseBytes > 0 {
		w.Limit(e.maxResponseBytes)
	}
	return w
}

// finishData settles the root writer once every task has finished. The size
// check reads the budget before any Reset, which clears it. Over the limit,
// the field errors are dropped along with the data they point into.
func (st *execState) finishData(ctx context.Context, w *jsonw.Writer, ok bool) {
	if limit := st.e.maxResponseBytes; limit > 0 && (w.LimitExceeded() || int64(w.Len()) > limit) {
		w.Reset()
		w.Null()
		st.errs = nil
		st.addError(ctx, Errorf("response exceeds the maximum size of %d bytes", limit).WithCode(CodeResponseTooLarge), nil, nil)
		return
	}
	if !ok {
		w.Reset()
		w.Null()
	}
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
	sortErrorsByDocumentOrder(st.errs)
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

	// order is this field's position in its selection set, which is document
	// order. It is what sorts the errors slice, since fields finish in
	// whatever order their resolvers return and every reference implementation
	// reports errors in document order. It occupies padding the struct already
	// had, so carrying it costs nothing.
	order int32
}

// materializeOrder returns the document-order sort key for this path: one
// element per segment, a field's position in its selection set or a list
// element's index. Siblings are either all fields or all elements, never a
// mix, so the two never have to be told apart.
func (n *pathNode) materializeOrder() []int32 {
	depth := 0
	for p := n; p != nil; p = p.parent {
		depth++
	}
	if depth == 0 {
		return nil
	}
	out := make([]int32, depth)
	for p := n; p != nil; p = p.parent {
		depth--
		if p.isIndex {
			out[depth] = int32(p.index)
		} else {
			out[depth] = p.order
		}
	}
	return out
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
	e *Executor
	// decision is carried per request so the write path can enforce each
	// field's Outcome by planField.authIdx with no lookup and no call back
	// into the Authorizer. It is nil when no Authorizer is configured or the
	// operation touches no declaring field, which keeps that path to one nil
	// check. The struct has no slack in its size class, so there is no
	// separate schema pointer: the schema is reached through e.
	decision *Decision
	vars     map[string]any

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

// addError presents err and appends it with the given path and location. It
// is for errors that explain why a request stopped, which WithMaxErrors never
// drops; field errors go through addFieldError.
func (st *execState) addError(ctx context.Context, err error, path Path, pos *ast.Position) {
	st.appendError(ctx, err, path, nil, pos, false)
}

// addFieldError records a field error unless the error limit is full. The
// check before presenting is what saves the work; the one under the lock in
// appendError is what makes the limit exact when concurrent fields race past
// the first.
func (st *execState) addFieldError(ctx context.Context, err error, path *pathNode, pos *ast.Position) {
	if st.fieldErrorsFull() {
		st.droppedFieldError(ctx, err)
		return
	}
	if !st.appendError(ctx, err, path.materialize(), path.materializeOrder(), pos, true) {
		st.droppedFieldError(ctx, err)
	}
}

// appendError reports false when a limited error was dropped because the
// list filled while it was being presented.
func (st *execState) appendError(ctx context.Context, err error, path Path, ord []int32, pos *ast.Position, limited bool) bool {
	presented := st.e.presenter(ctx, err)
	if presented == nil {
		return true
	}
	if presented.Path == nil {
		presented.Path = path
	}
	// Set even when the presenter supplied its own Path: the key describes
	// where the field was, which is what orders the list, and a presenter that
	// rewrites the path does not move the field.
	presented.ord = ord
	if pos != nil && len(presented.Locations) == 0 {
		presented.Locations = []Location{{Line: pos.Line, Column: pos.Column}}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if limited && st.fullLocked() {
		return false
	}
	st.errs = append(st.errs, presented)
	return true
}

// droppedFieldError keeps the reason a request stopped when the field error
// carrying it is dropped. A slow field often reports a timeout or
// cancellation only through its own error, with no later field reaching the
// checkpoint that records the engine's, so without this a full list could
// leave the response saying nothing about why it ended. recordCancellation
// adds at most one such error however many fields land here.
func (st *execState) droppedFieldError(ctx context.Context, err error) {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		st.recordCancellation(ctx, ctxErr)
	}
}

// fieldErrorsFull reports whether the field error limit is reached.
func (st *execState) fieldErrorsFull() bool {
	if st.e.maxErrors <= 0 {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.fullLocked()
}

// fullLocked reports whether the error limit is reached and, the first time
// it is, appends the notice. The notice needs no field of its own on
// execState, which has no room: past the limit only engine errors and the
// notice are ever appended, so a short scan of that tail finds it. A user field
// error carrying the same code cannot confuse it, since field errors are only
// ever appended below the limit.
func (st *execState) fullLocked() bool {
	limit := st.e.maxErrors
	if limit <= 0 || len(st.errs) < limit {
		return false
	}
	for _, e := range st.errs[limit:] {
		if e.Extensions["code"] == CodeErrorLimitExceeded {
			return true
		}
	}
	st.errs = append(st.errs, errorLimitNotice())
	return true
}

// fieldError records an error raised while producing the value of f. List
// indices carried by indexedError extend the path; errNonNull becomes the
// specification's non-null violation message.
func (st *execState) fieldError(ctx context.Context, err error, path *pathNode, f *planField, ord int32) {
	// Checked here as well as in addFieldError so that past the limit not even
	// the path nodes below are built.
	if st.fieldErrorsFull() {
		st.droppedFieldError(ctx, err)
		return
	}
	var soft *elementErrors
	if errors.As(err, &soft) {
		for _, ie := range soft.errs {
			st.fieldError(ctx, ie, path, f, ord)
		}
		return
	}
	full := path
	if f != nil {
		full = &pathNode{parent: path, key: f.alias, order: ord}
	}
	for {
		var ie *indexedError
		if !errors.As(err, &ie) {
			break
		}
		full = &pathNode{parent: full, index: ie.index, isIndex: true}
		err = ie.err
	}
	// A resolver that honours its context returns the bare deadline error,
	// which would reach the client as "context deadline exceeded" with no
	// code; say what actually happened.
	if errors.Is(err, context.DeadlineExceeded) && st.e.timedOut(ctx) {
		err = st.e.timeoutFieldError(err)
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
	st.addFieldError(ctx, err, full, pos)
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
