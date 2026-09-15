package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/syssam/graphql-go/internal/jsonw"
	"github.com/vektah/gqlparser/v2/ast"
)

// eventStream yields the events of one open subscription. ok is false once
// the source is exhausted; err is non-nil only when the context ended first.
type eventStream func(ctx context.Context) (event any, ok bool, err error)

// subscribeFunc opens the source stream for one subscription operation.
type subscribeFunc func(ctx context.Context, args any) (eventStream, error)

// errSubscriptionResolved guards the resolver of a subscription root field.
// The event is the field's value, so the per-event writer substitutes its own
// executor and this never runs; returning an error rather than a zero value
// means a future path that reaches it fails loudly instead of writing null.
var errSubscriptionResolved = errors.New("subscription root field is resolved from its event, not by a resolver")

// Subscribe binds a subscription root field to the function that opens its
// source event stream. Each value received becomes one response, with the
// value taking the place of the field's result, so the field's sub-selection
// is executed against it exactly as a query's would be.
//
// fn is called once per operation. It should return promptly and do its work
// in a goroutine feeding the channel; closing the channel ends the
// subscription. An error returned here prevents the subscription from
// starting. A channel carries no error of its own, so a source that can fail
// mid-stream should carry the failure in its event type.
func Subscribe[R any](name string, fn func(context.Context) (<-chan R, error), opts ...FieldOpt) FieldOption {
	return subscribeSpec[R](name, nil, opts, func(ctx context.Context, _ any) (<-chan R, error) {
		return fn(ctx)
	})
}

// SubscribeArgs binds a subscription root field that takes arguments decoded
// into A. An Args[A] registration must be present in the same schema.
func SubscribeArgs[A, R any](name string, fn func(context.Context, A) (<-chan R, error), opts ...FieldOpt) FieldOption {
	return subscribeSpec[R](name, reflect.TypeFor[A](), opts, func(ctx context.Context, args any) (<-chan R, error) {
		return fn(ctx, *args.(*A))
	})
}

// subscribeSpec composes a subscription field as an ordinary Root field so
// that argument decoding and output shape checking are the same code, then
// adds the stream opener on top.
func subscribeSpec[R any](name string, argsType reflect.Type, opts []FieldOpt, open func(context.Context, any) (<-chan R, error)) *fieldSpec {
	spec := newFieldSpec[Root, R](name, argsType, false, opts,
		func(context.Context, Root, any) (R, error) {
			var zero R
			return zero, errSubscriptionResolved
		})

	shape := spec.compose
	spec.compose = func(b *schemaBuilder, s *Schema, obj *objectType, def *ast.FieldDefinition, fd *fieldDef) error {
		if b.ast.Subscription == nil || obj.name != b.ast.Subscription.Name {
			return fmt.Errorf("field %s: Subscribe binds fields of the subscription root only", coordinate(obj.name, def.Name))
		}
		if err := shape(b, s, obj, def, fd); err != nil {
			return err
		}
		fd.subscribe = func(ctx context.Context, args any) (eventStream, error) {
			ch, err := open(ctx, args)
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context) (any, bool, error) {
				select {
				case v, ok := <-ch:
					if !ok {
						return nil, false, nil
					}
					return v, true, nil
				case <-ctx.Done():
					return nil, false, ctx.Err()
				}
			}, nil
		}
		return nil
	}
	return spec
}

// SubscribeError reports that a subscription could not be started: the
// document failed to parse or validate, variables were invalid, or the
// binding's stream opener returned an error. Response holds the errors in
// the form a transport sends them, which for the streaming protocols is a
// protocol-level error rather than a payload.
type SubscribeError struct {
	Response *Response
}

func (e *SubscribeError) Error() string {
	if e.Response == nil || len(e.Response.Errors) == 0 {
		return "graphql: subscription failed to start"
	}
	return "graphql: " + e.Response.Errors[0].Message
}

func (e *Executor) subscribeError(ctx context.Context, errs ...*Error) error {
	return &SubscribeError{Response: e.requestError(ctx, errs...)}
}

// Subscribe runs a subscription operation. It parses, validates and plans the
// document, opens the field's source stream, and returns a channel carrying
// one response per event. The channel is closed when the source closes or ctx
// is cancelled; cancelling ctx is how a caller unsubscribes.
//
// Responses are sent unbuffered, so a slow consumer applies backpressure to
// the source rather than accumulating events. Call Response.Release on each
// one after serializing it.
//
// On a request error nothing is started: the channel is nil and the error is
// a *SubscribeError carrying the response to send.
func (e *Executor) Subscribe(ctx context.Context, req *Request) (<-chan *Response, error) {
	start := time.Now()

	entry, errs := e.document(req.Query)
	if errs != nil {
		return nil, e.subscribeError(ctx, errs...)
	}
	op, oerr := selectOperation(entry.doc, req.OperationName)
	if oerr != nil {
		return nil, e.subscribeError(ctx, oerr)
	}
	if op.Operation != ast.Subscription {
		return nil, e.subscribeError(ctx, Errorf("Subscribe requires a subscription operation, got %s.", op.Operation).WithCode(CodeOperationResolution))
	}

	rawVars, err := decodeVariables(req.Variables)
	if err != nil {
		return nil, e.subscribeError(ctx, Errorf("%v", err).WithCode(CodeBadUserInput))
	}
	vars, verr := e.schema.coerceVariables(op, rawVars)
	if verr != nil {
		return nil, e.subscribeError(ctx, verr)
	}

	p, cacheHit, perrs := entry.planFor(e.schema, e, op, vars)
	if perrs != nil {
		return nil, e.subscribeError(ctx, perrs...)
	}

	// The validator enforces a single root selection, but @skip and @include
	// are folded per plan variant and can leave none.
	sel := p.sel.forType(p.root)
	if len(sel.fields) != 1 {
		return nil, e.subscribeError(ctx, Errorf("Subscription must select exactly one root field, this operation selects %d.", len(sel.fields)).WithCode(CodeValidationFailed))
	}
	f := sel.fields[0]
	if f.kind != fieldNormal || f.def == nil || f.def.subscribe == nil {
		return nil, e.subscribeError(ctx, Errorf("Subscription root field %s cannot be subscribed to.", f.name).WithCode(CodeOperationResolution))
	}

	base := &OperationContext{
		Operation:     op,
		Doc:           entry.doc,
		RawQuery:      req.Query,
		OperationName: req.OperationName,
		Variables:     vars,
		Stats:         OperationStats{Start: start, CacheHit: cacheHit},
		plan:          p,
		entry:         entry,
		hub:           newWaveCoordinator(),
	}
	// The stream opener runs under the base context; each event then gets its
	// own, installed below.
	ctx = withOperation(ctx, base)

	args := f.args
	if f.dynamicArgs {
		v, derr := f.def.args.decode(fieldArguments(f.ast, vars))
		if derr != nil {
			return nil, e.subscribeError(ctx, Errorf("Invalid argument for field %s: %v", coordinate(p.root.name, f.def.name), derr).WithCode(CodeBadUserInput))
		}
		args = v
	}

	stream, serr := f.def.subscribe(ctx, args)
	if serr != nil {
		return nil, e.subscribeError(ctx, Errorf("%v", serr).WithPath(Path{{Key: f.alias}}))
	}

	out := make(chan *Response)
	go e.pump(ctx, base, f, stream, out)
	return out, nil
}

// pump drains the source stream, executing the selection once per event.
func (e *Executor) pump(ctx context.Context, base *OperationContext, f *planField, stream eventStream, out chan<- *Response) {
	defer close(out)
	for {
		event, ok, err := stream(ctx)
		if err != nil || !ok {
			return
		}
		// The per-event context goes into ctx as well as into the chain. A
		// resolver reaches its operation through OperationFrom(ctx), so
		// passing only the chain argument would leave every event resolving
		// against the base: one shared DataLoader cache for the life of the
		// subscription, no wave coordinator to batch into, and extensions
		// written where no response ever reads them.
		oc := e.eventContext(base, f, event)
		resp := e.opChain(withOperation(ctx, oc), oc)
		select {
		case out <- resp:
		case <-ctx.Done():
			resp.Release()
			return
		}
	}
}

// eventContext builds the operation context for one event. Each event gets a
// fresh wave coordinator and value map: request-scoped extension state such
// as a DataLoader cache must not outlive the event that populated it, or a
// long-lived subscription would serve stale data forever.
func (e *Executor) eventContext(base *OperationContext, f *planField, event any) *OperationContext {
	return &OperationContext{
		Operation:     base.Operation,
		Doc:           base.Doc,
		RawQuery:      base.RawQuery,
		OperationName: base.OperationName,
		Variables:     base.Variables,
		Stats:         OperationStats{Start: time.Now(), CacheHit: base.Stats.CacheHit},
		plan:          base.plan,
		entry:         base.entry,
		hub:           newWaveCoordinator(),
		event:         &subEvent{field: f, value: event},
	}
}

// subEvent carries one event into the operation chain, so that interceptors,
// limits and cost accounting see every event as its own operation.
type subEvent struct {
	field *planField
	value any
}

// runSubscriptionEvent writes one event as a response. It substitutes the
// root field's executor with one that yields the event and then runs the
// ordinary object writer, so null bubbling, error paths and the sub-selection
// behave exactly as they do for a query.
//
// Substituting the executor also bypasses field interceptors and field
// directives on the subscription root field itself: there is no per-event
// resolver call for them to wrap. Both still observe every field beneath it.
func (e *Executor) runSubscriptionEvent(ctx context.Context, oc *OperationContext) *Response {
	src := oc.event.field
	event := oc.event.value
	fd := src.def

	f := *src
	if fd.leaf {
		writeAny, typ := fd.writeAny, fd.typ
		f.exec.writeLeaf = func(_ context.Context, w *jsonw.Writer, _, _ any) error {
			return writeAny(w, event, typ)
		}
	} else {
		f.exec.resolve = func(context.Context, any, any) (any, error) { return event, nil }
	}
	sel := &selectionSet{fields: []*planField{&f}}

	w := jsonw.Get()
	st := &execState{e: e, s: e.schema, vars: oc.Variables}
	if !st.writeObject(ctx, w, oc.plan.root, sel, &Root{}, nil, true) {
		w.Reset()
		w.Null()
	}
	return e.finishResponse(oc, w, st)
}
