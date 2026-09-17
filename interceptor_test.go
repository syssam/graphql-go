package graphql

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type recorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
}

func (r *recorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, " ")
}

// tagged implements all three interceptor interfaces and records the order
// in which they run.
type tagged struct {
	tag string
	rec *recorder
}

func (t tagged) InterceptRequest(ctx context.Context, req *Request, next RequestHandler) *Response {
	t.rec.add("req:" + t.tag)
	resp := next(ctx, req)
	t.rec.add("req-done:" + t.tag)
	return resp
}

func (t tagged) InterceptOperation(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
	t.rec.add("op:" + t.tag)
	return next(ctx, oc)
}

func (t tagged) InterceptField(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
	t.rec.add("field:" + t.tag + ":" + fc.Path().String())
	return next(ctx)
}

func TestInterceptorOrder(t *testing.T) {
	rec := &recorder{}
	_, e := newFixtureExecutor(t,
		WithRequestInterceptor(tagged{"a", rec}, tagged{"b", rec}),
		WithOperationInterceptor(tagged{"a", rec}, tagged{"b", rec}),
		WithFieldInterceptor(tagged{"a", rec}, tagged{"b", rec}),
	)
	resp := run(t, e, `{ me { id } }`, "")
	expectData(t, resp, `{"me":{"id":"1"}}`)
	want := "req:a req:b op:a op:b field:a:me field:b:me field:a:me.id field:b:me.id req-done:b req-done:a"
	if got := rec.joined(); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
}

func TestRequestInterceptorShortCircuitAndExtensions(t *testing.T) {
	deny := RequestInterceptorFunc(func(ctx context.Context, req *Request, next RequestHandler) *Response {
		if strings.Contains(req.Query, "fail") {
			return &Response{Errors: []*Error{Errorf("denied").WithCode("FORBIDDEN")}}
		}
		resp := next(ctx, req)
		if resp.Extensions == nil {
			resp.Extensions = map[string]any{}
		}
		resp.Extensions["trace"] = "t1"
		return resp
	})
	_, e := newFixtureExecutor(t, WithRequestInterceptor(deny))
	resp := run(t, e, `{ fail }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Message != "denied" || resp.Errors[0].Extensions["code"] != "FORBIDDEN" {
		t.Fatalf("got %s", errorsJSON(resp.Errors))
	}
	resp = run(t, e, `{ me { id } }`, "")
	out, _ := resp.MarshalJSON()
	if string(out) != `{"data":{"me":{"id":"1"}},"extensions":{"trace":"t1"}}` {
		t.Fatalf("got %s", out)
	}
}

func TestOperationInterceptorComplexityLimit(t *testing.T) {
	limit := OperationInterceptorFunc(func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
		oc.Set("seen", oc.Operation.Operation)
		if oc.Complexity() > 3 {
			return &Response{Errors: []*Error{Errorf("operation too complex: %d", oc.Complexity()).WithCode("COMPLEXITY_LIMIT")}}
		}
		resp := next(ctx, oc)
		if v, ok := oc.Get("seen"); !ok || v != oc.Operation.Operation {
			t.Errorf("operation values lost")
		}
		return resp
	})
	_, e := newFixtureExecutor(t, WithOperationInterceptor(limit))
	resp := run(t, e, `{ me { id name } }`, "")
	expectData(t, resp, `{"me":{"id":"1","name":"Alice"}}`)
	resp = run(t, e, `{ me { id name nick tags } }`, "")
	if !resp.HasRequestErrors() || !strings.HasPrefix(resp.Errors[0].Message, "operation too complex: 5") {
		t.Fatalf("got %s", errorsJSON(resp.Errors))
	}
}

func TestFieldInterceptorObservesAndRewrites(t *testing.T) {
	var count, leafs int
	var mu sync.Mutex
	obs := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		v, err := next(ctx)
		mu.Lock()
		count++
		if fc.Field.Name == "name" {
			leafs++
			v = strings.ToUpper(v.(string))
		}
		mu.Unlock()
		if fc.Object.Name != "User" && fc.Object.Name != "Query" {
			t.Errorf("unexpected object %s", fc.Object.Name)
		}
		return v, err
	})
	_, e := newFixtureExecutor(t, WithFieldInterceptor(obs))
	resp := run(t, e, `{ users { id name friends { name } } }`, "")
	expectData(t, resp, `{"users":[{"id":"1","name":"ALICE","friends":[{"name":"BOB"},{"name":"CAROL"}]},{"id":"2","name":"BOB","friends":[{"name":"ALICE"}]},{"id":"3","name":"CAROL","friends":[]}]}`)
	// users + 3×(id, name, friends) + 3 friend names.
	if count != 13 || leafs != 6 {
		t.Fatalf("count=%d leafs=%d", count, leafs)
	}
}

func TestFieldInterceptorErrorAndArgs(t *testing.T) {
	obs := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		if a, ok := fc.Args.(*idArgs); ok && a.ID == "blocked" {
			return nil, Errorf("blocked id").WithCode("BLOCKED")
		}
		return next(ctx)
	})
	_, e := newFixtureExecutor(t, WithFieldInterceptor(obs))
	resp := run(t, e, `{ user(id: "blocked") { id } }`, "")
	expectError(t, resp, `{"user":null}`, "user", "blocked id")
	if resp.Errors[0].Extensions["code"] != "BLOCKED" {
		t.Fatalf("extensions = %v", resp.Errors[0].Extensions)
	}
	resp = run(t, e, `{ user(id: "1") { id } }`, "")
	expectData(t, resp, `{"user":{"id":"1"}}`)
}

// TestFieldInterceptorSeesPureFieldsWithoutContextAttachment pins the split
// this change introduces: an interceptor still receives every field's
// FieldContext as its parameter, but the context no longer carries one for a
// pure field, because nothing but the interceptor could read it there --
// Field and FieldArgs accessors take no context at all.
func TestFieldInterceptorSeesPureFieldsWithoutContextAttachment(t *testing.T) {
	var seen []string
	var attached []bool
	_, e := newFixtureExecutor(t, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			if fc == nil {
				t.Error("interceptor got a nil FieldContext")
				return next(ctx)
			}
			seen = append(seen, fc.Object.Name+"."+fc.Field.Name)
			attached = append(attached, FieldFrom(ctx) != nil)
			return next(ctx)
		})))

	run(t, e, `{ users { id } }`, "")

	if len(seen) == 0 {
		t.Fatal("interceptor never ran")
	}
	for i, name := range seen {
		if name == "Query.users" {
			if !attached[i] {
				t.Errorf("%s: resolver field lost its FieldContext from the context", name)
			}
			continue // a resolver field: the context must still carry it
		}
		if attached[i] {
			t.Errorf("%s: context still carries a FieldContext for a pure field", name)
		}
	}
}

const directiveSDL = `
directive @upper on FIELD_DEFINITION
directive @prefix(with: String!, times: Int = 1) on FIELD_DEFINITION
type Item { name: String! @upper @prefix(with: "> ", times: 2) tags: [String!]! @upper }
type Query { item: Item! greeting: String! @prefix(with: "hi ") }
`

type dItem struct {
	Name string
	Tags []string
}

type prefixArgs struct {
	With  string
	Times *int
}

func upperValue(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ToUpper(x)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = strings.ToUpper(s)
		}
		return out
	}
	return v
}

func TestSchemaDirectivesWrapFields(t *testing.T) {
	s, err := NewSchema(SDL(directiveSDL),
		Args[prefixArgs](
			InputField("with", func(a *prefixArgs, v string) { a.With = v }),
			InputField("times", func(a *prefixArgs, v *int) { a.Times = v }),
		),
		Directive("upper", func(next FieldFunc) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				return upperValue(v), err
			}
		}),
		DirectiveArgs("prefix", func(next FieldFunc, a prefixArgs) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if err != nil {
					return nil, err
				}
				return strings.Repeat(a.With, *a.Times) + v.(string), nil
			}
		}),
		Object[dItem]("Item",
			Field("name", func(i *dItem) string { return i.Name }),
			Field("tags", func(i *dItem) []string { return i.Tags }),
		),
		Object[Root]("Query",
			Field("item", func(Root) *dItem { return &dItem{Name: "pen", Tags: []string{"a", "b"}} }),
			Field("greeting", func(Root) string { return "there" }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ item { name tags } greeting }`, "")
	// Directives wrap outermost-first in SDL order: prefix(upper(name)).
	expectData(t, resp, `{"item":{"name":"> > PEN","tags":["A","B"]},"greeting":"hi there"}`)
}

func TestSchemaDirectiveErrors(t *testing.T) {
	_, err := NewSchema(SDL(directiveSDL),
		Directive("missing", func(next FieldFunc) FieldFunc { return next }),
		DirectiveArgs("prefix", func(next FieldFunc, _ prefixArgs) FieldFunc { return next }),
		Object[dItem]("Item", Field("name", func(i *dItem) string { return i.Name }), Field("tags", func(i *dItem) []string { return i.Tags })),
		Object[Root]("Query", Field("item", func(Root) *dItem { return nil }), Field("greeting", func(Root) string { return "" })),
	)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{`Directive "missing": directive is not defined`, "no Args[graphql.prefixArgs] registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	_, err = NewSchema(SDL(directiveSDL),
		Directive("prefix", func(next FieldFunc) FieldFunc { return next }),
		Object[dItem]("Item", Field("name", func(i *dItem) string { return i.Name }), Field("tags", func(i *dItem) []string { return i.Tags })),
		Object[Root]("Query", Field("item", func(Root) *dItem { return nil }), Field("greeting", func(Root) string { return "" })),
	)
	if err == nil || !strings.Contains(err.Error(), "use DirectiveArgs") {
		t.Fatalf("Directive on an argumented directive must fail, got %v", err)
	}
}

const selectionSDL = `
type Child { id: ID! path: String! }
type Parent { id: ID! children(first: Int = 5): [Child!]! selected: [String!]! }
type Query { parent: Parent! }
`

type sParent struct{ ID string }
type sChild struct{ ID string }
type firstArgs struct{ First *int }

func TestSelectionAndPathInNestedResolvers(t *testing.T) {
	var childrenArgs any
	s, err := NewSchema(SDL(selectionSDL),
		Args[firstArgs](InputField("first", func(a *firstArgs, v *int) { a.First = v })),
		Object[sChild]("Child",
			Field("id", func(c *sChild) string { return c.ID }),
			Resolve("path", func(ctx context.Context, _ *sChild) (string, error) { return PathFrom(ctx).String(), nil }),
		),
		Object[sParent]("Parent",
			Field("id", func(p *sParent) string { return p.ID }),
			ResolveArgs("children", func(ctx context.Context, p *sParent, a firstArgs) ([]*sChild, error) {
				out := make([]*sChild, 0, *a.First)
				for i := range *a.First {
					out = append(out, &sChild{ID: p.ID + "-" + itoa(int64(i))})
				}
				return out, nil
			}),
			Resolve("selected", func(ctx context.Context, _ *sParent) ([]string, error) {
				return nil, nil
			}),
		),
		Object[Root]("Query",
			Resolve("parent", func(ctx context.Context, _ Root) (*sParent, error) {
				sel := SelectionFrom(ctx)
				var names []string
				for f := range sel.Fields() {
					names = append(names, f.Alias+"="+f.Name)
					if f.Name == "children" {
						childrenArgs = f.Args
					}
				}
				if sub, ok := sel.Sub("children"); !ok || !sub.Has("path") || sub.Has("id") {
					t.Errorf("Sub(children) = %v ok=%v", sub, ok)
				}
				if _, ok := sel.Sub("id"); ok {
					t.Error("leaf must not expose a sub-selection")
				}
				if strings.Join(names, ",") != "id=id,kids=children" {
					t.Errorf("selected fields = %v", names)
				}
				return &sParent{ID: "p"}, nil
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ parent { id kids: children(first: 2) { path } } }`, "")
	expectData(t, resp, `{"parent":{"id":"p","kids":[{"path":"parent.kids[0].path"},{"path":"parent.kids[1].path"}]}}`)
	if a, ok := childrenArgs.(*firstArgs); !ok || *a.First != 2 {
		t.Fatalf("children args = %#v", childrenArgs)
	}
	if !OperationFrom(context.Background()).isNil() {
		t.Fatal("OperationFrom outside a request must be nil")
	}
}

func (oc *OperationContext) isNil() bool { return oc == nil }
