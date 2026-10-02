package graphql

import (
	"context"
	"strings"
	"testing"
)

// DirectiveArgs applies FIELD_DEFINITION and OBJECT. A binding for a directive
// declared on neither can never wrap anything: the author writes a wrapper,
// the schema builds, and the wrapper is dead. That was a slog.Debug line --
// off by default -- so nothing told them.
//
// It is the same fail-open the requirement directives already reject
// misplacement to prevent, and the adjacent mistake, a binding for a directive
// the SDL does not declare at all, was already a build error. This makes the
// two consistent.

type dlArgs struct{ N int }

func TestADirectiveBindingThatCanNeverWrapIsABuildError(t *testing.T) {
	for _, c := range []struct{ name, locations string }{
		{"argument only", "ARGUMENT_DEFINITION"},
		{"input field only", "INPUT_FIELD_DEFINITION"},
		{"several, none usable", "ARGUMENT_DEFINITION | INPUT_FIELD_DEFINITION | ENUM_VALUE"},
		{"executable only", "FIELD | QUERY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(`
				directive @audit on `+c.locations+`
				type Query { ping: String! }
			`),
				Directive("audit", func(next FieldFunc) FieldFunc { return next }),
				Query(Field("ping", func(Root) string { return "pong" })),
			)
			if err == nil {
				t.Fatal("a binding that can never wrap a field was accepted; the author's " +
					"wrapper is dead and nothing says so")
			}
			if !strings.Contains(err.Error(), "audit") {
				t.Errorf("error does not name the directive: %v", err)
			}
			if !strings.Contains(err.Error(), c.locations[:strings.IndexAny(c.locations+" ", " ")]) {
				t.Errorf("error does not name the declared locations: %v", err)
			}
		})
	}
}

// One usable location among several is enough, because the binding then does
// something wherever it is applied. The directive is written where its
// declared locations allow, so every case actually runs rather than skipping.
func TestADirectiveBindingWithOneUsableLocationBuilds(t *testing.T) {
	for _, c := range []struct{ locations, sdl string }{
		{"FIELD_DEFINITION", "type Query { ping: String! @audit }"},
		{"OBJECT", "type Query @audit { ping: String! }"},
		{"FIELD_DEFINITION | ARGUMENT_DEFINITION", "type Query { ping: String! @audit }"},
		{"ARGUMENT_DEFINITION | OBJECT", "type Query @audit { ping: String! }"},
	} {
		t.Run(c.locations, func(t *testing.T) {
			var ran int
			s, err := NewSchema(SDL(`
				directive @audit on `+c.locations+`
				`+c.sdl+`
			`),
				Directive("audit", func(next FieldFunc) FieldFunc {
					return func(ctx context.Context, parent, args any) (any, error) {
						ran++
						return next(ctx, parent, args)
					}
				}),
				Query(Field("ping", func(Root) string { return "pong" })),
			)
			if err != nil {
				t.Fatalf("NewSchema: %v", err)
			}
			expectData(t, run(t, NewExecutor(s), `{ ping }`, ""), `{"ping":"pong"}`)
			if ran != 1 {
				t.Errorf("the directive ran %d times, want 1: it was bound and applied, "+
					"so it has to wrap the field", ran)
			}
		})
	}
}

type ifaceDirNode interface{ isIfaceDirNode() }

type ifaceDirUser struct{}

func (*ifaceDirUser) isIfaceDirNode() {}

// FIELD_DEFINITION is a location this engine applies, and an interface's field
// is written at it. The wrapper was attached to object fields only, so a
// directive on an interface field built cleanly and never ran: a schema that
// guards Node.secret that way served it through every implementer.
func TestABoundDirectiveOnAnInterfaceFieldIsABuildError(t *testing.T) {
	_, err := NewSchema(SDL(`
		directive @adminOnly on FIELD_DEFINITION
		interface Node { secret: String! @adminOnly }
		type User implements Node { secret: String! }
		type Query { node: Node! }
	`),
		Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }),
		Interface[ifaceDirNode]("Node"),
		Object[ifaceDirUser]("User", Field("secret", func(*ifaceDirUser) string { return "s3cr3t" })),
		Query(Field("node", func(Root) ifaceDirNode { return &ifaceDirUser{} })),
	)
	if err == nil {
		t.Fatal("a bound directive on an interface field was accepted; it wraps nothing and nothing says so")
	}
	for _, want := range []string{"Node.secret", "@adminOnly", "User.secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// Written on the implementing type's own field as well, the directive runs
// there, so the interface's copy is documentation and the schema builds.
func TestABoundDirectiveOnAnInterfaceFieldBuildsWhenEveryImplementerCarriesIt(t *testing.T) {
	var ran int
	s, err := NewSchema(SDL(`
		directive @adminOnly on FIELD_DEFINITION
		interface Node { secret: String! @adminOnly }
		type User implements Node { secret: String! @adminOnly }
		type Query { node: Node! }
	`),
		Directive("adminOnly", func(next FieldFunc) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				ran++
				return next(ctx, parent, args)
			}
		}),
		Interface[ifaceDirNode]("Node"),
		Object[ifaceDirUser]("User", Field("secret", func(*ifaceDirUser) string { return "s3cr3t" })),
		Query(Field("node", func(Root) ifaceDirNode { return &ifaceDirUser{} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ node { secret } }`, ""), `{"node":{"secret":"s3cr3t"}}`)
	if ran != 1 {
		t.Fatalf("the directive ran %d times, want 1", ran)
	}
}

// A directive declared for several locations builds as long as one of them is
// applied, and may then be written at the others. Each such use wrapped
// nothing: @adminOnly on an argument, an input field or an enum value read as
// a guard and was not one. Where a bound directive is written is checked use
// by use, and each one that cannot run is named.
func TestABoundDirectiveWrittenWhereItNeverRunsIsABuildError(t *testing.T) {
	const locations = `FIELD_DEFINITION | OBJECT | ARGUMENT_DEFINITION | INPUT_FIELD_DEFINITION | INPUT_OBJECT | ENUM | ENUM_VALUE | UNION | SCALAR | INTERFACE | SCHEMA`
	for name, c := range map[string]struct{ sdl, want string }{
		"a field argument": {
			`type Query { ping(secret: String @adminOnly): String! }`, "Query.ping(secret:)",
		},
		"an interface field's argument": {
			`interface Node { ping(secret: String @adminOnly): String! }
			 type Query implements Node { ping(secret: String): String! }`, "Node.ping(secret:)",
		},
		"an input field": {
			`input Filter { secret: String @adminOnly }
			 type Query { ping(f: Filter): String! }`, "Filter.secret",
		},
		"an input object": {
			`input Filter @adminOnly { secret: String }
			 type Query { ping(f: Filter): String! }`, "Filter",
		},
		"an enum": {
			`enum Level @adminOnly { LOW HIGH }
			 type Query { ping(l: Level): String! }`, "Level",
		},
		"an enum value": {
			`enum Level { LOW HIGH @adminOnly }
			 type Query { ping(l: Level): String! }`, "Level.HIGH",
		},
		"a scalar": {
			`scalar Token @adminOnly
			 type Query { ping(t: Token): String! }`, "Token",
		},
		"the schema": {
			`schema @adminOnly { query: Query }
			 type Query { ping: String! }`, "schema",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewSchema(SDL("directive @adminOnly on "+locations+"\n"+c.sdl),
				Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }))
			if err == nil {
				t.Fatal("a bound directive written where nothing applies it was accepted")
			}
			var found bool
			for line := range strings.SplitSeq(err.Error(), "\n") {
				if strings.Contains(line, "@adminOnly") && strings.Contains(line, c.want) && strings.Contains(line, "never runs") {
					found = true
				}
			}
			if !found {
				t.Errorf("no error names @adminOnly at %s as never running:\n%v", c.want, err)
			}
		})
	}
}

// An interface or a union has no executor of its own. Written on one, a bound
// directive runs only where each object behind it carries it too, which is
// the rule an interface's field already had.
func TestABoundDirectiveOnAnAbstractTypeMustBeOnEveryObjectBehindIt(t *testing.T) {
	build := func(sdl string) error {
		abstract := Interface[ifaceDirNode]("Node")
		if strings.HasPrefix(sdl, "union") {
			abstract = Union[ifaceDirNode]("Node")
		}
		_, err := NewSchema(SDL("directive @adminOnly on OBJECT | INTERFACE | UNION\n"+sdl),
			Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }),
			abstract,
			Object[ifaceDirUser]("User", Field("secret", func(*ifaceDirUser) string { return "s3cr3t" })),
			Query(Field("node", func(Root) ifaceDirNode { return &ifaceDirUser{} })),
		)
		return err
	}
	for name, sdl := range map[string]string{
		"an interface": `interface Node @adminOnly { secret: String! }
			type User implements Node { secret: String! }
			type Query { node: Node! }`,
		"a union": `union Node @adminOnly = User
			type User { secret: String! }
			type Query { node: Node! }`,
	} {
		t.Run(name, func(t *testing.T) {
			err := build(sdl)
			if err == nil {
				t.Fatal("a bound directive on an abstract type was accepted, and User is served without it")
			}
			for _, want := range []string{"@adminOnly", "Node", "User"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %s: %v", want, err)
				}
			}
		})
		t.Run(name+", carried by the object", func(t *testing.T) {
			if err := build(strings.Replace(strings.Replace(sdl, "type User implements Node {", "type User implements Node @adminOnly {", 1), "type User {", "type User @adminOnly {", 1)); err != nil {
				t.Fatalf("the object carries the directive, so it runs there: %v", err)
			}
		})
	}
}

// A built-in directive means something at every place the specification
// declares it for. Binding one -- @deprecated, to count calls to deprecated
// fields -- is no reason to refuse it on an enum value or an argument, where
// it is the only way to say the thing is deprecated; the uses check did, and
// schemas that had built stopped building.
func TestBindingABuiltInDirectiveLeavesItsOtherUsesAlone(t *testing.T) {
	var ran int
	s, err := NewSchema(SDL(`
		enum Level { LOW HIGH @deprecated }
		type Query { ping(old: Int @deprecated, l: Level): String! @deprecated }
	`),
		DirectiveArgs("deprecated", func(next FieldFunc, _ struct{ Reason *string }) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				ran++
				return next(ctx, parent, args)
			}
		}),
		Enum[string]("Level", map[string]string{"LOW": "LOW", "HIGH": "HIGH"}),
		Query(FieldArgs("ping", func(Root, struct {
			Old *int
			L   *string
		}) string {
			return "pong"
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ ping }`, ""), `{"ping":"pong"}`)
	if ran != 1 {
		t.Errorf("the wrapper ran %d times, want 1", ran)
	}
}

// One mistake is reported once, with advice that can be followed.
func TestADirectiveMistakeIsReportedOnce(t *testing.T) {
	bind := Directive("adminOnly", func(next FieldFunc) FieldFunc { return next })
	ping := Query(Field("ping", func(Root) string { return "pong" }))
	count := func(t *testing.T, err error) int {
		t.Helper()
		if err == nil {
			t.Fatal("accepted")
		}
		n := 0
		for line := range strings.SplitSeq(err.Error(), "\n") {
			if strings.Contains(line, "adminOnly") {
				n++
			}
		}
		return n
	}
	// Declared for nowhere this engine applies: that is the one thing to
	// say, not that and each use.
	t.Run("a directive that can wrap nothing, and is used", func(t *testing.T) {
		_, err := NewSchema(SDL(`directive @adminOnly on ARGUMENT_DEFINITION
			type Query { ping(a: Int @adminOnly): String! }`), bind,
			Query(FieldArgs("ping", func(Root, struct{ A *int }) string { return "pong" })))
		if n := count(t, err); n != 1 {
			t.Errorf("%d errors for one mistake:\n%v", n, err)
		}
	})
	t.Run("a repeatable directive written twice at one place", func(t *testing.T) {
		_, err := NewSchema(SDL(`directive @adminOnly repeatable on FIELD_DEFINITION | ENUM
			enum Level @adminOnly @adminOnly { LOW }
			type Query { ping: String! }`), bind, ping)
		if n := count(t, err); n != 1 {
			t.Errorf("%d errors for one place:\n%v", n, err)
		}
	})
}

// An interface's directive runs where each object behind it carries it: on
// the object, or -- when the directive is not declared for OBJECT, and so
// cannot be written there -- on each of the interface's fields in it.
func TestAnAbstractTypesDirectiveMayBeCarriedByTheObjectsFields(t *testing.T) {
	build := func(user string) error {
		_, err := NewSchema(SDL(`directive @adminOnly on INTERFACE | FIELD_DEFINITION
			interface Node @adminOnly { secret: String! }
			`+user+`
			type Query { node: Node! }`),
			Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }),
			Interface[ifaceDirNode]("Node"),
			Object[ifaceDirUser]("User", Field("secret", func(*ifaceDirUser) string { return "s3cr3t" })),
			Query(Field("node", func(Root) ifaceDirNode { return &ifaceDirUser{} })),
		)
		return err
	}
	err := build(`type User implements Node { secret: String! }`)
	if err == nil {
		t.Fatal("User.secret is served without the interface's directive")
	}
	// OBJECT is not a location this directive has, so "write it on User"
	// would be advice nobody can follow.
	if !strings.Contains(err.Error(), "User.secret") {
		t.Errorf("the error does not say where to write it: %v", err)
	}
	if err := build(`type User implements Node { secret: String! @adminOnly }`); err != nil {
		t.Fatalf("every field of the interface carries the directive in User: %v", err)
	}
}

// The subscription root has a rule of its own -- nothing wraps its fields --
// and reaching it through an interface went around that rule one way and was
// given advice it then refused the other.
func TestABoundDirectiveReachingTheSubscriptionRootThroughAnInterface(t *testing.T) {
	build := func(sdl string) error {
		_, err := NewSchema(SDL(sdl),
			Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }),
			Interface[any]("I"),
			Query(Field("ping", func(Root) string { return "pong" })),
			Subscription(Subscribe("tick", func(context.Context) (<-chan int, error) { return nil, nil })),
		)
		return err
	}
	for name, sdl := range map[string]string{
		"on the interface's field": `directive @adminOnly on FIELD_DEFINITION
			interface I { tick: Int! @adminOnly }
			type Subscription implements I { tick: Int! }
			type Query { ping: String! }`,
		"on the interface": `directive @adminOnly on OBJECT | INTERFACE
			interface I @adminOnly { tick: Int! }
			type Subscription implements I { tick: Int! }
			type Query { ping: String! }`,
	} {
		t.Run(name, func(t *testing.T) {
			err := build(sdl)
			if err == nil {
				t.Fatal("accepted, and the subscription root is served without it")
			}
			if !strings.Contains(err.Error(), "SubscriptionInterceptor") {
				t.Errorf("the error does not say what a subscription root uses instead: %v", err)
			}
			if strings.Contains(err.Error(), "write it on Subscription") {
				t.Errorf("the error gives advice the build then refuses: %v", err)
			}
		})
	}
}

// Carrying an interface's directive on the fields is for a directive that
// cannot be written on the object. One that can has to be, because the
// object's other fields are reached through the interface too -- node { ...
// on User { extra } } -- and a directive on the interface reads as covering
// what is behind it.
func TestAnAbstractTypesDirectiveIsCarriedByFieldsOnlyWhereItCannotBeOnTheObject(t *testing.T) {
	_, err := NewSchema(SDL(`directive @adminOnly on INTERFACE | OBJECT | FIELD_DEFINITION
		interface Node @adminOnly { secret: String! }
		type User implements Node { secret: String! @adminOnly extra: String! }
		type Query { node: Node! }`),
		Directive("adminOnly", func(next FieldFunc) FieldFunc { return next }),
		Interface[ifaceDirNode]("Node"),
		Object[ifaceDirUser]("User",
			Field("secret", func(*ifaceDirUser) string { return "s3cr3t" }),
			Field("extra", func(*ifaceDirUser) string { return "also" })),
		Query(Field("node", func(Root) ifaceDirNode { return &ifaceDirUser{} })),
	)
	if err == nil {
		t.Fatal("User.extra is served without the directive its interface carries")
	}
	if !strings.Contains(err.Error(), "write it on User as well") {
		t.Errorf("the error does not say to write it on the object: %v", err)
	}
}

// A built-in is exempt on the subscription root as everywhere else: binding
// @deprecated is no reason to refuse a deprecated subscription field, written
// there or on an interface the root implements.
func TestABoundBuiltInIsAllowedOnTheSubscriptionRoot(t *testing.T) {
	for name, sdl := range map[string]string{
		"on the root's field": `type Subscription { tick: Int! @deprecated }
			type Query { ping: String! }`,
		"on an interface's field": `interface I { tick: Int! @deprecated }
			type Subscription implements I { tick: Int! }
			type Query { ping: String! }`,
	} {
		t.Run(name, func(t *testing.T) {
			opts := []SchemaOption{
				DirectiveArgs("deprecated", func(next FieldFunc, _ struct{ Reason *string }) FieldFunc { return next }),
				Query(Field("ping", func(Root) string { return "pong" })),
				Subscription(Subscribe("tick", func(context.Context) (<-chan int, error) { return nil, nil })),
			}
			if strings.Contains(sdl, "interface I") {
				opts = append(opts, Interface[any]("I"))
			}
			if _, err := NewSchema(SDL(sdl), opts...); err != nil {
				t.Fatalf("NewSchema: %v", err)
			}
		})
	}
}
