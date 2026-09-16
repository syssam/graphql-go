package graphql

import (
	"strings"
	"testing"
)

type tPost struct{ ID string }

type tOther struct{ ID string }

type node interface{ nodeID() string }

func (u *tUser) nodeID() string  { return u.ID }
func (p *tPost) nodeID() string  { return p.ID }
func (o *tOther) nodeID() string { return o.ID }

const abstractSDL = `
interface Node { id: ID! }
type User implements Node { id: ID! }
type Post implements Node { id: ID! }
type Other { id: ID! }
union SearchResult = User | Post
type Query { node: Node search: [SearchResult!]! anyNode: Node }
`

func abstractObjects() []SchemaOption {
	return []SchemaOption{
		Object[tUser]("User", Field("id", func(u *tUser) string { return u.ID })),
		Object[tPost]("Post", Field("id", func(p *tPost) string { return p.ID })),
		Object[tOther]("Other", Field("id", func(o *tOther) string { return o.ID })),
	}
}

func TestAbstractDynamicResolution(t *testing.T) {
	s, err := NewSchema(SDL(abstractSDL),
		append(abstractObjects(),
			Object[Root]("Query",
				Field("node", func(Root) node { return &tUser{ID: "u"} }),
				Field("search", func(Root) []any { return nil }),
				Field("anyNode", func(Root) *tPost { return nil }),
			),
		)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	nodeT := s.abstracts["Node"]
	obj, err := s.concreteType(nodeT, &tUser{})
	if err != nil || obj.name != "User" {
		t.Fatalf("got %v, %v", obj, err)
	}
	obj, err = s.concreteType(nodeT, &tPost{})
	if err != nil || obj.name != "Post" {
		t.Fatalf("got %v, %v", obj, err)
	}
	if _, err := s.concreteType(nodeT, &Root{}); err == nil {
		t.Fatal("unbound Go type must fail")
	}
	if _, err := s.concreteType(nodeT, &tOther{}); err == nil || !strings.Contains(err.Error(), "possible types") {
		t.Fatalf("bound non-member must fail, got %v", err)
	}
}

func TestAbstractTypeResolver(t *testing.T) {
	s, err := NewSchema(SDL(abstractSDL),
		append(abstractObjects(),
			Interface[node]("Node"),
			Union[any]("SearchResult", TypeResolver(func(v any) string {
				if _, ok := v.(*tPost); ok {
					return "Post"
				}
				return "Nope"
			})),
			Object[Root]("Query",
				Field("node", func(Root) node { return nil }),
				Field("search", func(Root) []any { return nil }),
				Field("anyNode", func(Root) node { return nil }),
			),
		)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	sr := s.abstracts["SearchResult"]
	obj, err := s.concreteType(sr, &tPost{})
	if err != nil || obj.name != "Post" {
		t.Fatalf("TypeResolver: %v %v", obj, err)
	}
	if _, err := s.concreteType(sr, &tUser{}); err == nil || !strings.Contains(err.Error(), `"Nope"`) {
		t.Fatalf("unknown resolved name must fail, got %v", err)
	}
	if s.abstracts["Node"].goType == nil {
		t.Fatal("Interface binding must record the Go type")
	}
}

func TestAbstractShapeChecks(t *testing.T) {
	err := Validate(SDL(abstractSDL),
		append(abstractObjects(),
			Object[Root]("Query",
				Field("node", func(Root) *tOther { return nil }),
				Field("search", func(Root) []any { return nil }),
				Field("anyNode", func(Root) node { return nil }),
			),
		)...,
	)
	if err == nil || !strings.Contains(err.Error(), "not a possible type of Node") {
		t.Fatalf("concrete non-member should fail, got %v", err)
	}

	err = Validate(SDL(abstractSDL),
		append(abstractObjects(),
			Object[Root]("Query",
				Field("node", func(Root) node { return nil }),
				Field("search", func(Root) []any { return nil }),
				Field("anyNode", func(Root) node { return nil }),
			),
			Union[any]("Node"),
		)...,
	)
	if err == nil || !strings.Contains(err.Error(), `Union "Node"`) {
		t.Fatalf("Union on an interface should fail, got %v", err)
	}

	err = Validate(SDL(abstractSDL),
		append(abstractObjects(),
			Object[Root]("Query",
				Field("node", func(Root) node { return nil }),
				Field("search", func(Root) []any { return nil }),
				Field("anyNode", func(Root) node { return nil }),
			),
			Interface[node]("Node"),
			Interface[node]("Node"),
		)...,
	)
	if err == nil || !strings.Contains(err.Error(), "bound more than once") {
		t.Fatalf("duplicate abstract binding should fail, got %v", err)
	}
}

// A union list is traversed by the same machinery as an object list, so the
// seq spelling must be indistinguishable here too.
func TestSeqAbstractListMatchesSlice(t *testing.T) {
	_, e := newFixtureExecutor(t)
	slice := run(t, e, `{search(term:"a"){__typename}}`, "")
	seq := run(t, e, `{searchSeq(term:"a"){__typename}}`, "")
	if len(seq.Errors) != 0 {
		t.Fatalf("seq union list errored: %v", seq.Errors)
	}
	want := strings.Replace(string(slice.Data), `"search"`, `"searchSeq"`, 1)
	if got := string(seq.Data); got != want {
		t.Fatalf("seq union list = %s, want %s", got, want)
	}
}
