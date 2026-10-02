package graphql

import (
	"encoding/json"
	"math"
	"time"
)

// QueryCost is a Shopify / GitHub-style cost model computed from the
// compiled plan and variable values, before resolvers run.
//
// A field costs FieldWeight[Type.field] (default 1) plus the cost of its
// sub-selection multiplied by the list size. List size is the largest
// positive ListArguments value sent (default "first", "last"), or
// DefaultListSize when the field returns a list and neither argument is
// present. __typename is free, and so are __schema and __type with all they
// select: see WithMaxDepth.
type QueryCost struct {
	// Max is the maximum allowed cost. Zero means unlimited.
	Max int
	// DefaultListSize is used when a list field has no first/last.
	// Zero is treated as 10.
	DefaultListSize int
	// ListArguments names the arguments that bound a connection or list.
	// Empty means first, last.
	ListArguments []string
	// FieldWeight overrides the default weight of 1 for a schema
	// coordinate such as "Query.search".
	FieldWeight map[string]int
	// Report writes extensions.cost on every response, including operations
	// rejected by Max. Operations refused by WithMaxDepth/WithMaxComplexity
	// are rejected before a plan exists and so carry no cost.
	Report bool
	// Connections prices a Relay connection by the page size it asked for.
	// first/last sit on the connection field, one level above the edges list
	// they bound, so without this the arguments are invisible to the cost and
	// every connection is charged DefaultListSize whether it asked for one
	// element or a thousand.
	//
	// It is off by default because turning it on changes the number an
	// existing deployment has set its Max against.
	Connections bool
	// Actual also reports actualQueryCost, summed from the fields that were
	// really resolved rather than from assumed list sizes. Requested cost has
	// to guess how long a list will be; actual cost counts it, so the two
	// together say whether DefaultListSize is set anywhere near reality.
	//
	// It costs one integer add per resolved field on the write path, which is
	// why it is separate from Report rather than implied by it.
	Actual bool
}

func (c QueryCost) listArgs() []string {
	if len(c.ListArguments) == 0 {
		return []string{"first", "last"}
	}
	return c.ListArguments
}

func (c QueryCost) defaultList() int {
	if c.DefaultListSize <= 0 {
		return 10
	}
	return c.DefaultListSize
}

func (c QueryCost) weight(coord string) int {
	if c.FieldWeight != nil {
		if w, ok := c.FieldWeight[coord]; ok {
			return w
		}
	}
	return 1
}

// WithMaxComplexity rejects operations whose static field count (aliases
// included) exceeds n. Zero, the default, means unlimited. __schema and
// __type count as one field each: see WithMaxDepth.
func WithMaxComplexity(n int) ExecutorOption {
	return func(e *Executor) { e.maxComplexity = n }
}

// WithMaxDepth rejects operations whose selection nesting exceeds n.
// Zero, the default, means unlimited.
//
// __schema and __type count as one level, whatever they select, here and in
// WithMaxComplexity and QueryCost. These limits are set against an API's own
// queries, and the introspection query IDEs and client generators send nests
// its type references seven levels deep, so charging it would lock the tools
// out of any API that sets them. Introspection is bounded instead by
// graphql-js's MaxIntrospectionDepth rule: a path may pass through at most two
// of fields, interfaces, possibleTypes and inputFields. A public API should
// still DisableIntrospection.
func WithMaxDepth(n int) ExecutorOption {
	return func(e *Executor) { e.maxDepth = n }
}

// WithMaxResponseBytes bounds the size of each response's data: one query or
// mutation, or one subscription event. A response over the limit has null
// data and a single RESPONSE_TOO_LARGE error in place of any other errors,
// and execution stops resolving fields once it sees the limit passed.
//
// A response is rejected when its finished data is over the limit, or when
// the limit was passed at any point while executing: execution was cut short
// from then on, so the result is rejected even if null bubbling later
// discarded the bytes that passed it. While executing, a field other than
// __typename is checked before it is written, so each buffer being written
// concurrently can pass the limit by about one field's output before execution
// notices. The errors list is not counted. Zero means unlimited. The default
// is 64 MiB, which measured no cost distinguishable from no limit.
func WithMaxResponseBytes(n int64) ExecutorOption {
	return func(e *Executor) { e.maxResponseBytes = n }
}

// WithMaxErrors bounds how many errors one response carries. Past n, field
// errors are dropped, and a single ERROR_LIMIT_EXCEEDED error is appended in
// their place; the data is unaffected. A dropped error is not presented or
// given a path, except that concurrent fields racing the limit may be
// presented before they are dropped, so a presenter that logs can run a few
// more than n times. Errors that explain why a request stopped (cancellation,
// timeout, an oversized response) are always kept. Request errors from
// parsing and validation are cut the same way. Which field errors are kept is
// the order they were recorded in, which for concurrent fields is not
// deterministic. Zero means unlimited. The default is 1000: generous enough
// that a list with a failed field on each of hundreds of rows keeps every
// error, bounded enough that a million failing elements do not become a
// million errors in memory.
func WithMaxErrors(n int) ExecutorOption {
	return func(e *Executor) { e.maxErrors = n }
}

// WithMaxTokens caps the tokens a document may have: parsing stops at the
// limit and the request is refused with GRAPHQL_PARSE_FAILED. Parsing and
// validation run before WithMaxDepth, WithMaxComplexity and WithQueryCost
// can, on every document, valid or not, and gqlparser's default rules are
// not all linear in it, so this and WithMaxNesting are what bound that work.
// Zero means unlimited, which leaves gqlparser's recursive parser bounded
// only by the request body: about a million levels of [ overflows the
// goroutine stack, which no recover catches, and the transports' 1 MiB
// default body keeps a document under that. The default is 15 000, gqlgen's
// and Apollo Router's.
func WithMaxTokens(n int) ExecutorOption {
	return func(e *Executor) { e.maxTokens = n }
}

// WithMaxNesting caps how deeply a document's selection sets, or its input
// value literals, may nest as written: { a { b } } and [[1]] are both 2. A
// deeper document is refused with GRAPHQL_PARSE_FAILED before validation,
// whose ValuesOfCorrectType check does work at every level proportional to
// what is below it. It is not WithMaxDepth: that counts an operation's
// depth after fragments are expanded, this counts the text. Zero means
// unlimited. The default is 100.
func WithMaxNesting(n int) ExecutorOption {
	return func(e *Executor) { e.maxNesting = n }
}

// WithOperationTimeout bounds how long one operation may run: a query or
// mutation from the moment Execute is called, parsing and every interceptor
// included, or one subscription event, never a stream as a whole. Opening a
// subscription is not bounded either — its interceptors, authorization and
// source opener share the context the stream lives on, which a deadline
// would end. Each entry of an HTTP batch is its own operation, so a batch of
// n may take n times the timeout.
//
// At the deadline the operation's context is done, so fields not yet started
// are skipped and resolvers that honour their context return; the response
// keeps the data already written, with OPERATION_TIMEOUT errors where work was
// cut. A resolver's own error is kept in that error's chain, with its
// extensions. It does not force a response out on time: the executor waits
// for every resolver it started, so one that ignores its context still holds
// the response until it returns.
//
// Only this executor's own deadline is OPERATION_TIMEOUT. When a deadline the
// caller set ends execution, the executor reports REQUEST_CANCELLED, and a
// resolver's error for it is passed through unchanged. Zero, the default,
// means no timeout: the right bound depends on the workload, and a wrong
// default would cut short legitimate slow operations.
func WithOperationTimeout(d time.Duration) ExecutorOption {
	return func(e *Executor) { e.operationTimeout = d }
}

// WithQueryCost enables Shopify-style query cost. When c.Max > 0,
// operations above the cap are rejected before execution.
func WithQueryCost(c QueryCost) ExecutorOption {
	return func(e *Executor) {
		cp := c
		e.cost = &cp
	}
}

func (e *Executor) rejectIfOverLimit(oc *OperationContext) *Error {
	if oc.plan == nil {
		return nil
	}
	if e.maxComplexity > 0 && oc.plan.complexity > e.maxComplexity {
		return Errorf("query exceeds complexity limit: %d > %d", oc.plan.complexity, e.maxComplexity).
			WithCode(CodeTooComplex).
			WithExtension("complexity", oc.plan.complexity).
			WithExtension("maxComplexity", e.maxComplexity)
	}
	if e.maxDepth > 0 && oc.plan.depth > e.maxDepth {
		return Errorf("query exceeds maximum depth: %d > %d", oc.plan.depth, e.maxDepth).
			WithCode(CodeMaxDepth).
			WithExtension("depth", oc.plan.depth).
			WithExtension("maxDepth", e.maxDepth)
	}
	return e.rejectIfOverCost(oc)
}

// rejectIfOverCost is the one limit that depends on variables, so it cannot
// be applied before the plan is compiled the way depth and complexity are.
func (e *Executor) rejectIfOverCost(oc *OperationContext) *Error {
	if e.cost == nil || e.cost.Max <= 0 {
		return nil
	}
	if n := oc.ensureCost(*e.cost); n > e.cost.Max {
		return Errorf("query exceeds cost limit: %d > %d", n, e.cost.Max).
			WithCode(CodeTooComplex).
			WithExtension("requestedQueryCost", n).
			WithExtension("maxQueryCost", e.cost.Max)
	}
	return nil
}

// rejectByMetrics applies the static limits to metrics computed before the
// plan is compiled. rejectIfOverLimit applies the same two limits per request
// from the cached plan; this one exists so that a query over the limit is
// never compiled at all.
func (e *Executor) rejectByMetrics(m planMetrics) *Error {
	if e.maxComplexity > 0 && m.complexity > e.maxComplexity {
		return Errorf("query exceeds complexity limit: %d > %d", m.complexity, e.maxComplexity).
			WithCode(CodeTooComplex).
			WithExtension("complexity", m.complexity).
			WithExtension("maxComplexity", e.maxComplexity)
	}
	if e.maxDepth > 0 && m.depth > e.maxDepth {
		return Errorf("query exceeds maximum depth: %d > %d", m.depth, e.maxDepth).
			WithCode(CodeMaxDepth).
			WithExtension("depth", m.depth).
			WithExtension("maxDepth", e.maxDepth)
	}
	return nil
}

func (e *Executor) attachCost(oc *OperationContext, resp *Response) {
	if e.cost == nil || !e.cost.Report || oc == nil || oc.plan == nil {
		return
	}
	n := oc.ensureCost(*e.cost)
	payload := map[string]any{"requestedQueryCost": n}
	if e.cost.Max > 0 {
		payload["maxQueryCost"] = e.cost.Max
	}
	// An operation rejected before execution resolved nothing, so reporting a
	// zero would read as "this query was free" rather than "it never ran".
	if e.cost.Actual && oc.actualOK {
		payload["actualQueryCost"] = int(oc.actualCost)
	}
	if resp.Extensions == nil {
		resp.Extensions = map[string]any{}
	}
	resp.Extensions["cost"] = payload
}

func (oc *OperationContext) ensureCost(cfg QueryCost) int {
	if oc.costOK {
		return oc.costValue
	}
	oc.costValue = queryCostOf(oc.plan.sel, costWalk{vars: oc.Variables, cfg: cfg})
	oc.costOK = true
	return oc.costValue
}

// costKey memoizes the cost walk. It carries paid as well as the selection
// set: the same *selectionSet costs differently paid and unpaid, so a memo
// keyed on the pointer alone would hand the second visit whatever the first
// one computed.
type costKey struct {
	sel  *selectionSet
	paid bool
}

// costWalk is the state of one cost walk.
type costWalk struct {
	vars map[string]any
	cfg  QueryCost
	// paid reports that an enclosing connection's page size has already
	// counted the elements of a list at this level.
	//
	// The memo below keys on it, and the reason that is insurance rather than
	// a live bug is one line further down: childPaid is set in exactly one
	// place, and only for a field that is not itself a list. So a selection
	// set shared between two visits is the sub-selection of one AST field
	// node, and paid for it follows from that node alone. Add a second place
	// that sets it, or set it for a list, and the key stops being insurance.
	paid bool
	// memo keeps the walk linear in the plan's DAG rather than in the tree
	// that DAG unfolds to. It lives for exactly one request, because vars and
	// cfg are fixed only within one, and it is shared through the recursion
	// because copying a costWalk copies the map header rather than the map.
	memo map[costKey]int
}

func (w costWalk) withPaid(paid bool) costWalk {
	w.paid = paid
	return w
}

func queryCostOf(sel *selectionSet, w costWalk) int {
	if w.memo == nil {
		w.memo = make(map[costKey]int)
	}
	return queryCostMemo(sel, w)
}

func queryCostMemo(sel *selectionSet, w costWalk) int {
	if sel == nil {
		return 0
	}
	key := costKey{sel: sel, paid: w.paid}
	if n, ok := w.memo[key]; ok {
		return n
	}
	sum := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n = costAdd(n, fieldCost(f, w))
		}
		return n
	}
	n := 0
	if sel.byType == nil {
		n = sum(sel.fields)
	} else {
		for _, concrete := range sel.byType {
			n = max(n, sum(concrete.fields))
		}
	}
	w.memo[key] = n
	return n
}

// fieldCost prices one field. w.paid is what stops first: 200 from
// multiplying with the edges list's own default and pricing a page of 200 as
// one of 2000: the connection's page size counted those elements already.
func fieldCost(f *planField, w costWalk) int {
	if f.kind == fieldTypename || (f.def != nil && isIntrospectionRoot(f.def.name)) {
		return 0
	}
	weight := 1
	if f.def != nil && f.def.object != nil {
		weight = w.cfg.weight(coordinate(f.def.object.name, f.def.name))
	}
	// A leaf multiplies nothing, so its multiplier need not be computed.
	// Working one out means reading arguments off every scalar field in the
	// query, and scalars are most of the fields in most queries.
	if f.sub == nil {
		return weight
	}

	mult, childPaid := 1, false
	switch isList := f.def != nil && f.def.typ != nil && f.def.typ.Elem != nil; {
	case isList && w.paid:
		// Paid for by the connection above, unless it names a page size of
		// its own: then it is not that connection's edges but a second page,
		// and the client chose its length. Left at one, items(first: 500)
		// cost what items(first: 5) did.
		if n, ok := pageArg(f, w); ok {
			mult = n
		}
	case isList:
		mult = listMultiplier(f, w)
	case w.cfg.Connections:
		if n, ok := pageArg(f, w); ok {
			mult, childPaid = n, true
		}
	}

	// f.sub is non-nil: a leaf returned above.
	return costAdd(weight, costMul(queryCostMemo(f.sub, w.withPaid(childPaid)), mult))
}

// costAdd and costMul saturate rather than wrap. A client picks the page
// sizes, so three nested first: 2147483647 lists are one request away, and a
// wrapped product is negative: under any Max, and a credit to ext/throttle.
func costAdd(a, b int) int {
	switch {
	case b > 0 && a > math.MaxInt-b:
		return math.MaxInt
	case b < 0 && a < math.MinInt-b:
		return math.MinInt
	}
	return a + b
}

// costMul expects m >= 1, which every page size and DefaultListSize is.
func costMul(a, m int) int {
	switch {
	case a > 0 && a > math.MaxInt/m:
		return math.MaxInt
	case a < 0 && a < math.MinInt/m:
		return math.MinInt
	}
	return a * m
}

func listMultiplier(f *planField, w costWalk) int {
	if n, ok := pageArg(f, w); ok {
		return n
	}
	return w.cfg.defaultList()
}

// pageArg returns the largest positive pagination argument on f, reporting
// whether there was one. Only a positive value counts, so first: 0 is priced
// like no first at all, at the default list size: that errs high for a
// request that asks for nothing, never low. The largest wins because a request
// that sends both first and last must be priced at the bigger page, never the
// smaller one the client happened to name first.
func pageArg(f *planField, w costWalk) (int, bool) {
	if f.ast == nil {
		return 0, false
	}
	best, found := 0, false
	for _, name := range w.cfg.listArgs() {
		a := f.ast.Arguments.ForName(name)
		if a == nil {
			continue
		}
		raw, err := a.Value.Value(w.vars)
		if err != nil {
			continue
		}
		if n, ok := asCostInt(raw); ok && n > 0 && (!found || n > best) {
			best, found = n, true
		}
	}
	return best, found
}

func asCostInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		// Int coercion accepts 5e2 and 500.0 (rawInt64), so the price must
		// too: a page size it cannot read here is priced as DefaultListSize
		// while the resolver still receives the real one.
		f, err := n.Float64()
		if err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
		return floatCostInt(f)
	case float64:
		return floatCostInt(n)
	default:
		return 0, false
	}
}

// floatCostInt clamps before converting, because converting a float64 beyond
// the range of int, or NaN, is implementation defined in Go.
func floatCostInt(n float64) (int, bool) {
	switch {
	case math.IsNaN(n):
		return 0, false
	case n >= math.MaxInt:
		return math.MaxInt, true
	case n <= math.MinInt:
		return math.MinInt, true
	}
	return int(n), true
}
