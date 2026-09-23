package graphql

import (
	"context"
	"strings"
	"testing"
)

// json:"-" means the field is not on the wire, and Input[T] has to agree: an
// ORM marks its internal fields that way, and deriving a GraphQL name from the
// Go field instead fails the build against a schema that never declared it.
// One real ent-derived WhereInput carries Predicates []predicate.X `json:"-"`,
// and that alone was 447 errors out of 12 283.
func TestInputSkipsJSONDashFields(t *testing.T) {
	const sdl = `
input Filter { name: String }
type Query { search(f: Filter): String! }
`
	s, err := NewSchema(SDL(sdl),
		Input[filterInput]("Filter"),
		Query(ResolveArgs("search", func(_ context.Context, _ Root, a searchArgs) (string, error) {
			if a.F == nil {
				return "none", nil
			}
			if a.F.Name == nil {
				return "nil", nil
			}
			return *a.F.Name, nil
		})),
		Args[searchArgs](),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	resp := e.Execute(context.Background(), &Request{Query: `{ search(f: {name: "ada"}) }`})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"search":"ada"}` {
		t.Fatalf("got %s", got)
	}
}

type searchArgs struct {
	F *filterInput
}

type filterInput struct {
	Predicates []string `json:"-"`
	Name       *string  `json:"name,omitempty"`
}

// The escape hatch encoding/json documents: json:"-," is a field really named
// "-". Rare, but reading it as "skip" would silently drop a declared field.
func TestInputHonoursTheJSONDashComaEscape(t *testing.T) {
	type odd struct {
		// staticcheck SA5008 flags this tag as ambiguous, which is the whole
		// point: json:"-," is encoding/json's documented escape for a field
		// really named "-", and the engine has to tell it from json:"-".
		Dash *string `json:"-,"` //nolint:staticcheck
	}
	const sdl = `
input Odd { "-": String }
type Query { q(o: Odd): String! }
`
	_, err := NewSchema(SDL(sdl), Input[odd]("Odd"))
	if err != nil && strings.Contains(err.Error(), "has no binding") {
		t.Fatalf(`json:"-," was read as skip: %v`, err)
	}
}
