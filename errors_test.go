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
