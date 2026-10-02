package graphql

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"testing"
)

type panicDate string

type panicDateArgs struct{ D *panicDate }

type panicFilter struct{ Name string }

type panicFilterArgs struct{ F *panicFilter }

type panicPet interface{ petName() string }

type panicDog struct{ Name string }

func (d *panicDog) petName() string { return d.Name }

const panicSDL = `
scalar Date
interface Pet { name: String! }
type Dog implements Pet { name: String! }
input Filter { name: String }
type Query {
  in(d: Date): String
  inF(f: Filter): String
  pet: Pet
  rows: [Dog!]
  ping: String!
}
type Subscription { ticks: Int! }`

func panicExecutor(t *testing.T, opts ...ExecutorOption) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(panicSDL),
		// The ordinary slip: a type assertion on input the client chose.
		Scalar("Date",
			func(w *Writer, d panicDate) error { w.String(string(d)); return nil },
			func(v any) (panicDate, error) { return panicDate(v.(string)), nil }),
		Args[panicDateArgs](),
		Input[panicFilter]("Filter", InputField("name", func(*panicFilter, *string) { panic("setter boom") })),
		Args[panicFilterArgs](),
		Interface[panicPet]("Pet", TypeResolver(func(panicPet) string { panic("type resolver boom") })),
		Object[panicDog]("Dog", Field("name", func(d *panicDog) string { return d.Name })),
		Query(
			FieldArgs("in", func(Root, panicDateArgs) *string { return nil }),
			FieldArgs("inF", func(Root, panicFilterArgs) *string { return nil }),
			Field("pet", func(Root) panicPet { return &panicDog{Name: "rex"} }),
			// The body runs after the resolver has returned, which is what puts
			// it outside the resolver's own recover.
			Resolve("rows", func(context.Context, Root) (iter.Seq[*panicDog], error) {
				return func(func(*panicDog) bool) { panic("row scan boom") }, nil
			}),
			Field("ping", func(Root) string { return "pong" }),
		),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) {
			return make(chan int), nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s, opts...)
}

// WithRecover used to cover the resolver call and nothing else, on the
// reasoning that Execute runs on the caller's goroutine and the transport
// recovers there. gqlws and gqlfiber's WebSocket run each operation on a
// goroutine of their own, so every one of these ended the process -- the first
// three driven by nothing but the request's own input.
func TestExecutePanicOutsideAResolverIsAnInternalError(t *testing.T) {
	boom := func() { panic("interceptor boom") }
	for _, tc := range []struct {
		name  string
		opts  []ExecutorOption
		query string
		vars  string
	}{
		{name: "scalar unmarshal, literal", query: `{ in(d: 5) }`},
		{name: "scalar unmarshal, variable", query: `query($d: Date) { in(d: $d) }`, vars: `{"d": 5}`},
		{name: "input field setter", query: `{ inF(f: {name: "x"}) }`},
		{name: "input field setter, variable", query: `query($f: Filter) { inF(f: $f) }`, vars: `{"f": {"name": "x"}}`},
		{name: "type resolver", query: `{ pet { name } }`},
		{name: "iter.Seq body", query: `{ rows { name } }`},
		{name: "request interceptor", query: `{ ping }`, opts: []ExecutorOption{
			WithRequestInterceptor(RequestInterceptorFunc(func(context.Context, *Request, RequestHandler) *Response {
				boom()
				return nil
			})),
		}},
		{name: "operation interceptor", query: `{ ping }`, opts: []ExecutorOption{
			WithOperationInterceptor(OperationInterceptorFunc(func(context.Context, *OperationContext, OperationHandler) *Response {
				boom()
				return nil
			})),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := panicExecutor(t, tc.opts...)
			resp := run(t, e, tc.query, tc.vars)
			if resp.Data != nil {
				t.Fatalf("data = %s, want none: the operation did not finish", resp.Data)
			}
			if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeInternal {
				t.Fatalf("errors = %s, want one INTERNAL_SERVER_ERROR", errorsJSON(resp.Errors))
			}
			if msg := resp.Errors[0].Message; msg != "internal system error" {
				t.Fatalf("message = %q: the panic value must not reach the client", msg)
			}
			// The executor is still usable: nothing was left locked. The
			// interceptor cases panic on every request, so they have no
			// request left to ask with.
			if len(tc.opts) == 0 {
				expectData(t, run(t, e, `{ ping }`, ""), `{"ping":"pong"}`)
			}
		})
	}
}

// Opening a subscription runs the subscription interceptors on the caller's
// goroutine too, which over a WebSocket is the operation's own.
func TestSubscribePanicWhileOpeningIsASubscribeError(t *testing.T) {
	e := panicExecutor(t, WithSubscriptionInterceptor(SubscriptionInterceptorFunc(
		func(context.Context, *OperationContext, SubscriptionHandler) (<-chan *Response, error) {
			panic("interceptor boom")
		})))
	out, err := e.Subscribe(context.Background(), &Request{Query: `subscription { ticks }`})
	if out != nil {
		t.Fatal("a stream was returned for a subscription whose opening panicked")
	}
	var se *SubscribeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *SubscribeError", err)
	}
	errs := se.Response.Errors
	if len(errs) != 1 || errs[0].Extensions["code"] != CodeInternal || errs[0].Message != "internal system error" {
		t.Fatalf("errors = %s, want one INTERNAL_SERVER_ERROR", errorsJSON(errs))
	}
}

// Disabling recovery is how a test asks for the panic itself.
func TestExecutePanicSurfacesWithRecoverDisabled(t *testing.T) {
	e := panicExecutor(t, WithRecover(false))
	defer func() {
		if recover() == nil {
			t.Fatal("WithRecover(false) swallowed the panic")
		}
	}()
	run(t, e, `{ in(d: 5) }`, "")
}

func boomTypeResolver(panicPet) string { panic("type resolver boom") }

// A panic on a scheduled field's goroutine is carried to the goroutine that
// waits for it and raised again there, which is where Execute recovers it --
// and where the stack was then taken. The log line was the only record of the
// panic, and it showed the wait and none of the code that panicked.
func TestAPanicOnAScheduledFieldIsLoggedWithItsOwnStack(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	s, err := NewSchema(SDL(`
		interface Pet { name: String! }
		type Dog implements Pet { name: String! }
		type Query { a: Pet b: Pet }
	`),
		Interface[panicPet]("Pet", TypeResolver(boomTypeResolver)),
		Object[panicDog]("Dog", Field("name", func(d *panicDog) string { return d.Name })),
		Query(
			// Two resolver fields, so each is written on a goroutine of its own.
			Resolve("a", func(context.Context, Root) (panicPet, error) { return &panicDog{}, nil }),
			Resolve("b", func(context.Context, Root) (panicPet, error) { return &panicDog{}, nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := run(t, NewExecutor(s), `{ a { name } b { name } }`, "")
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeInternal {
		t.Fatalf("errors = %s, want one INTERNAL_SERVER_ERROR", errorsJSON(resp.Errors))
	}
	if !strings.Contains(logged.String(), "boomTypeResolver") {
		t.Fatalf("the logged stack does not reach the function that panicked:\n%s", logged.String())
	}
}
