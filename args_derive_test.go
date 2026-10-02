package graphql

import (
	"context"
	"strings"
	"testing"
)

type derivedArgs struct {
	X int
	Y *string
}

// FieldArgs and ResolveArgs name the argument struct in their own signature,
// and an empty Args[A]() says nothing they do not: it derives every field from
// the struct. Requiring it anyway was a second registration to forget, with an
// error that did not say what to add. An argument struct nobody registered is
// derived the same way.
func TestAnArgumentStructIsDerivedWithoutArgs(t *testing.T) {
	ticks := make(chan int, 1)
	s, err := NewSchema(SDL(`
		directive @prefix(with: String!) on FIELD_DEFINITION
		type Query { pure(x: Int!, y: String): Int! io(x: Int!, y: String): Int! tagged: String! @prefix(with: "> ") }
		type Subscription { ticks(x: Int!, y: String): Int! }
	`),
		DirectiveArgs("prefix", func(next FieldFunc, a struct{ With string }) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if err != nil {
					return nil, err
				}
				return a.With + v.(string), nil
			}
		}),
		Query(
			FieldArgs("pure", func(_ Root, a derivedArgs) int { return a.X }),
			ResolveArgs("io", func(_ context.Context, _ Root, a derivedArgs) (int, error) { return a.X + len(*a.Y), nil }),
			Field("tagged", func(Root) string { return "pen" }),
		),
		Subscription(SubscribeArgs("ticks", func(_ context.Context, a derivedArgs) (<-chan int, error) {
			ticks <- a.X
			close(ticks)
			return ticks, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	expectData(t, run(t, e, `{ pure(x: 3) io(x: 1, y: "ab") tagged }`, ""), `{"pure":3,"io":3,"tagged":"> pen"}`)
	expectData(t, run(t, e, `query($x: Int!) { pure(x: $x) }`, `{"x": 9}`), `{"pure":9}`)

	out, serr := e.Subscribe(context.Background(), &Request{Query: `subscription { ticks(x: 5) }`})
	if serr != nil {
		t.Fatalf("subscribe: %v", serr)
	}
	expectData(t, <-out, `{"ticks":5}`)
}

// Deriving is still checked against the schema, so a struct that does not fit
// the field's arguments is a build error naming the argument, not a request
// that decodes into the wrong thing.
func TestADerivedArgumentStructMustFitTheField(t *testing.T) {
	for name, tc := range map[string]struct {
		field FieldOption
		want  string
	}{
		"a field the schema has no argument for": {
			ResolveArgs("q", func(context.Context, Root, struct{ Z int }) (*int, error) { return nil, nil }),
			`argument "z" is not defined`,
		},
		"an argument the struct has no field for": {
			ResolveArgs("q", func(context.Context, Root, struct{ Y *string }) (*int, error) { return nil, nil }),
			`argument "x" has no binding`,
		},
		"an argument type that is not a struct": {
			ResolveArgs("q", func(context.Context, Root, int) (*int, error) { return nil, nil }),
			"decoded into a struct",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(SDL(`type Query { q(x: Int!, y: String): Int }`), Query(tc.field))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "Query.q") {
				t.Errorf("error does not name the field: %v", err)
			}
		})
	}
}

// An explicit registration is what the field uses, derived or not.
func TestAnExplicitArgsRegistrationStillDecides(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { q(x: Int!, y: String): Int! }`),
		Args[derivedArgs](
			InputField("x", func(a *derivedArgs, v int) { a.X = v * 10 }),
			InputField("y", func(a *derivedArgs, v *string) { a.Y = v }),
		),
		Query(FieldArgs("q", func(_ Root, a derivedArgs) int { return a.X })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ q(x: 3) }`, ""), `{"q":30}`)
}
