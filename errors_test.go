package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func TestPathJSONAndString(t *testing.T) {
	p := Path{{Key: "user"}, {Key: "posts"}, {Index: 2, IsIndex: true}, {Key: "title"}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `["user","posts",2,"title"]`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
	if got, want := p.String(), "user.posts[2].title"; got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
}

func TestErrorMarshalOmitsEmptyMembers(t *testing.T) {
	b, err := json.Marshal(Errorf("boom %d", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"message":"boom 1"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestErrorMarshalFull(t *testing.T) {
	e := Errorf("x").WithCode(CodeInternal).WithPath(Path{{Key: "a"}, {Index: 0, IsIndex: true}})
	e.Locations = []Location{{Line: 1, Column: 3}}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"message":"x","locations":[{"line":1,"column":3}],"path":["a",0],"extensions":{"code":"INTERNAL_SERVER_ERROR"}}`
	if got := string(b); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestErrorStringIncludesPath(t *testing.T) {
	e := Errorf("bad").WithPath(Path{{Key: "me"}, {Key: "name"}})
	if got := e.Error(); got != "me.name: bad" {
		t.Fatalf("Error() = %q", got)
	}
}

type codedErr struct{ msg string }

func (e codedErr) Error() string { return e.msg }
func (e codedErr) GraphQLExtensions() map[string]any {
	return map[string]any{"code": "CUSTOM", "retry": true}
}

func TestDefaultErrorPresenterUnwraps(t *testing.T) {
	inner := Errorf("inner").WithCode("X")
	out := DefaultErrorPresenter(context.Background(), fmt.Errorf("wrap: %w", inner))
	if out.Message != "inner" || out.Extensions["code"] != "X" {
		t.Fatalf("presenter did not unwrap: %+v", out)
	}
	if out == inner {
		t.Fatal("presenter must return a copy, not the shared error")
	}
	out.WithExtension("k", 1)
	if _, leaked := inner.Extensions["k"]; leaked {
		t.Fatal("annotating the presented error mutated the original")
	}
}

func TestDefaultErrorPresenterMergesExtensions(t *testing.T) {
	out := DefaultErrorPresenter(context.Background(), fmt.Errorf("ctx: %w", codedErr{"boom"}))
	if out.Message != "ctx: boom" {
		t.Fatalf("Message = %q", out.Message)
	}
	if out.Extensions["code"] != "CUSTOM" || out.Extensions["retry"] != true {
		t.Fatalf("extensions not merged: %v", out.Extensions)
	}
}

func TestDefaultErrorPresenterPlainError(t *testing.T) {
	cause := errors.New("plain")
	out := DefaultErrorPresenter(context.Background(), cause)
	if out.Message != "plain" || out.Err != cause || out.Extensions != nil {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestDefaultErrorPresenterGQLError(t *testing.T) {
	src := &gqlerror.Error{
		Message:   "syntax",
		Locations: []gqlerror.Location{{Line: 2, Column: 5}},
		Path:      ast.Path{ast.PathName("a"), ast.PathIndex(1)},
	}
	out := DefaultErrorPresenter(context.Background(), src)
	if out.Message != "syntax" || len(out.Locations) != 1 || out.Locations[0].Line != 2 {
		t.Fatalf("unexpected: %+v", out)
	}
	if got := out.Path.String(); got != "a[1]" {
		t.Fatalf("path = %s", got)
	}
}

// writeList hands each element its path node from a chunked slab, and those
// nodes must stay the element's own after the loop has moved on: a
// FieldContext keeps only the parent pointer and materializes the path when
// asked, which a resolver or interceptor may do at any time. A slab that
// reused one node would answer every one of them with the last index. The
// list spans several chunks so the reuse a single chunk could hide shows up
// too. Recording the errors is not enough to catch this -- addFieldError
// materializes the path there and then -- so the paths are read afterwards.
func TestListElementPathNodesSurviveTheLoop(t *testing.T) {
	const n = 300
	var kept []*FieldContext
	s, err := NewSchema(SDL(`type Query { items: [Item] } type Item { v: String }`),
		Object[Root]("Query", Resolve("items", func(context.Context, Root) ([]*pathItem, error) {
			out := make([]*pathItem, n)
			for i := range out {
				out[i] = &pathItem{}
			}
			return out, nil
		})),
		// A pure Field keeps the list on writeList's sequential path, which is
		// the only one the slab serves; a resolver would schedule instead.
		Object[pathItem]("Item", Field("v", func(*pathItem) string { return "x" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	keep := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		if fc.Field.Name == "v" {
			kept = append(kept, fc)
		}
		return next(ctx)
	})

	run(t, NewExecutor(s, WithFieldInterceptor(keep)), `{ items { v } }`, "")
	if len(kept) != n {
		t.Fatalf("intercepted %d fields, want %d", len(kept), n)
	}
	for want, fc := range kept {
		p := fc.Path()
		if len(p) != 3 || p[0].Key != "items" || !p[1].IsIndex || p[2].Key != "v" {
			t.Fatalf("field %d path = %s, want items[i].v", want, p)
		}
		if p[1].Index != want {
			t.Fatalf("field %d reports index %d; the slab handed out a shared node", want, p[1].Index)
		}
	}
}

type pathItem struct{}
