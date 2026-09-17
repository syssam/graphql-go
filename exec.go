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
	operationTimeout time.Duration
	// timeoutCause is the context cause of this executor's own deadline, nil
	// without one. Compared by identity, it tells the timeout apart from a
	// deadline the caller set.
	timeoutCause error
	cost         *QueryCost
	authorizer   Authorizer
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
// INTERNAL_SERVER_ERROR field errors, and Authorizer panics into the same
// error for the whole operation or event. It is enabled by default; disable it
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

		maxResponseBytes: 64 << 20,
		maxErrors:        1000,
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
		resp.Errors = append(resp.Errors, e.presenter(ctx, err))
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
func (e *Executor) authorize(ctx context.Context, p *plan) (*Decision, error) {
	if e.authorizer == nil || p.shape.IsEmpty() {
		return nil, nil
	}
	return e.runAuthorizer(ctx, p)
}

// runAuthorizer is split from authorize so the disabled path carries no
// deferred recover. The recover matters beyond tidiness: per-event
// authorization runs in pump's goroutine, where a panicking policy client
// would end the process rather than one request.
func (e *Executor) runAuthorizer(ctx context.Context, p *plan) (d *Decision, err error) {
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
	decision, err := e.authorize(ctx, p)
	if err != nil {
		return e.requestError(ctx, authorizerError(ctx, err))
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
	st.appendError(ctx, err, path, pos, false)
}

// addFieldError records a field error unless the error limit is full. The
// check before presenting is what saves the work; the one under the lock in
// appendError is what makes the limit exact when concurrent fields race past
// the first.
func (st *execState) addFieldError(ctx context.Context, err error, path *pathNode, pos *ast.Position) {
	if st.fieldErrorsFull() {
		return
	}
	st.appendError(ctx, err, path.materialize(), pos, true)
}

func (st *execState) appendError(ctx context.Context, err error, path Path, pos *ast.Position, limited bool) {
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
	defer st.mu.Unlock()
	if limited && st.fullLocked() {
		return
	}
	st.errs = append(st.errs, presented)
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
// notice are ever appended, so a short scan of that tail finds it.
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
