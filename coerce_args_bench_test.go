package graphql

import (
	"context"
	"testing"
)

// fieldArguments is on the request path for every field carrying arguments --
// once per request for dynamic ones (exec_object.go) and once per plan compile
// for constant ones (plan.go) -- and no benchmark reached it. The engine
// benchmarks all use argument-free fields, so a change there measured as
// "no regression" against them would be measuring nothing.

type benchArgs struct {
	First *int
	Name  *string
	Tag   *string
}

func benchArgsExec(t testing.TB) *Executor {
	s, err := NewSchema(SDL(`
		type Query { rows(first: Int, name: String, tag: String = "t"): String! }
	`),
		Args[benchArgs](
			InputField("first", func(a *benchArgs, v *int) { a.First = v }),
			InputField("name", func(a *benchArgs, v *string) { a.Name = v }),
			InputField("tag", func(a *benchArgs, v *string) { a.Tag = v }),
		),
		Query(ResolveArgs("rows", func(_ context.Context, _ Root, a benchArgs) (string, error) {
			return "ok", nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// Constant arguments: decoded once at plan compile, then reused. The benchmark
// therefore measures the cached path, which is what a served request pays.
func BenchmarkExecuteConstantArgs(b *testing.B) {
	e := benchArgsExec(b)
	req := &Request{Query: `{ rows(first: 10, name: "ada") }`}
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

// Variable arguments: dynamicArgs, so fieldArguments and the decode run on
// every request. This is the one that measures the change.
func BenchmarkExecuteVariableArgs(b *testing.B) {
	e := benchArgsExec(b)
	req := &Request{
		Query:     `query($f: Int, $n: String){ rows(first: $f, name: $n) }`,
		Variables: []byte(`{"f":10,"n":"ada"}`),
	}
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

// A fresh plan each time, so the constant-argument decode at plan compile is
// what dominates rather than being amortised away.
func BenchmarkPlanCompileWithConstantArgs(b *testing.B) {
	s, err := NewSchema(SDL(`
		type Query { rows(first: Int, name: String, tag: String = "t"): String! }
	`),
		Args[benchArgs](
			InputField("first", func(a *benchArgs, v *int) { a.First = v }),
			InputField("name", func(a *benchArgs, v *string) { a.Name = v }),
			InputField("tag", func(a *benchArgs, v *string) { a.Tag = v }),
		),
		Query(ResolveArgs("rows", func(_ context.Context, _ Root, a benchArgs) (string, error) {
			return "ok", nil
		})),
	)
	if err != nil {
		b.Fatal(err)
	}
	req := &Request{Query: `{ rows(first: 10, name: "ada") }`}
	b.ReportAllocs()
	for b.Loop() {
		e := NewExecutor(s, WithPlanCache(0))
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}
