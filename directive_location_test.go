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
