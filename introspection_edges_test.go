package graphql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Introspection is what GraphiQL, Apollo Studio, schema registries and every
// codegen tool send on connect, so what comes back has to be well formed for
// shapes the schema author did not think about.
//
// introspection.go's other uncovered branch -- the includeDeprecated filter in
// __schema.directives -- is dead by construction and says so in place twice:
// gqlparser's AST has nowhere to hang a directive applied to a directive
// definition, so introDirective.directives() returns nil and nothing is ever
// filtered. There is no test to write for it and it is not an oversight.

// A wrapper type -- [Row!]! and its inner [Row!] and Row! -- has no definition
// of its own, so description, and everything else read off a definition, has
// to answer null rather than reach through a nil. A query asking for it is
// ordinary: "give me every field's type, with its description".
func TestIntrospectionDescriptionOnAWrapperTypeIsNull(t *testing.T) {
	s, err := NewSchema(SDL(`
		"a row"
		type Row { id: ID! }
		type Query { grid: [[Row!]!]! }
	`),
		Object[struct{ ID string }]("Row",
			Field("id", func(r *struct{ ID string }) ID { return ID(r.ID) }),
		),
		Query(Resolve("grid", func(context.Context, Root) ([][]*struct{ ID string }, error) {
			return nil, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := NewExecutor(s).Execute(context.Background(), &Request{Query: `{
		__type(name: "Query") {
			fields {
				type { kind name description
					ofType { kind name description
						ofType { kind name description
							ofType { kind name description
								ofType { kind name description
									ofType { kind name description } } } } } }
			}
		}
	}`})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}

	// Walk the ofType chain rather than counting levels by hand: the property
	// is "a level with no name is a wrapper and has no description", and it
	// holds at whatever depth [[Row!]!]! unfolds to.
	var body struct {
		Type struct {
			Fields []struct{ Type *introProbe } `json:"fields"`
		} `json:"__type"`
	}
	if err := json.Unmarshal(resp.Data, &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Data)
	}
	if len(body.Type.Fields) != 1 {
		t.Fatalf("want one field, got %d", len(body.Type.Fields))
	}

	wrappers, named := 0, 0
	for tp := body.Type.Fields[0].Type; tp != nil; tp = tp.OfType {
		if tp.Name == nil {
			wrappers++
			if tp.Description != nil {
				t.Errorf("a %s wrapper carries description %q; a wrapper has no "+
					"definition to read one off", tp.Kind, *tp.Description)
			}
			continue
		}
		named++
		if *tp.Name != "Row" {
			t.Errorf("the named type is %q, want Row", *tp.Name)
		}
		if tp.Description == nil || *tp.Description != "a row" {
			t.Errorf("the named type lost its description: %v", tp.Description)
		}
	}
	if wrappers == 0 {
		t.Error("no wrapper types in the chain, so nothing was tested")
	}
	if named != 1 {
		t.Errorf("%d named types at the bottom of the chain, want 1", named)
	}
	t.Logf("%d wrappers, %d named", wrappers, named)
}

// introProbe decodes one level of the __Type ofType chain.
type introProbe struct {
	Kind        string      `json:"kind"`
	Name        *string     `json:"name"`
	Description *string     `json:"description"`
	OfType      *introProbe `json:"ofType"`
}

// TestIntrospectionStringDescriptionHasItsSpace pins a repair to gqlparser's
// prelude, which reads "The `String`scalar type" with no space. Every
// introspection response carries the description and every tool that renders
// schema documentation shows it, so the engine repairs it rather than waiting
// on upstream. Found by diffing a full introspection response against
// graphql-js, which is the only way a missing space in a built-in description
// ever surfaces.
func TestIntrospectionStringDescriptionHasItsSpace(t *testing.T) {
	_, e := newFixtureExecutor(t)
	resp := run(t, e, `{ __type(name: "String") { description } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	got := string(resp.Data)
	if strings.Contains(got, "`String`scalar") {
		t.Fatalf("the prelude's missing space reached introspection: %s", got)
	}
	if !strings.Contains(got, "The `String` scalar type represents textual data") {
		t.Fatalf("description = %s", got)
	}
}
