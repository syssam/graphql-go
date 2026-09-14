package benchmarks

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/benchmarks/internal/data"
)

func execGraphQLGo(e *graphql.Executor, query string) []byte {
	resp := e.Execute(context.Background(), &graphql.Request{Query: query})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		panic(resp.Errors[0])
	}
	b, err := resp.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return b
}

func TestEnginesProduceSameJSON(t *testing.T) {
	users := data.Dataset()
	gg := newGraphQLGo(users)
	gn := newGQLGen(users)
	for _, q := range []string{queryShallow, queryNested} {
		got := compactJSON(execGraphQLGo(gg, q))
		want := compactJSON(gn.execute(context.Background(), q))
		if !bytes.Equal(got, want) {
			t.Fatalf("query %s JSON mismatch\ngraphql-go: %s\ngqlgen:     %s", q, got, want)
		}
	}
}

func compactJSON(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func BenchmarkGraphQLGoShallow(b *testing.B) { benchGraphQLGo(b, queryShallow) }
func BenchmarkGQLGenShallow(b *testing.B)    { benchGQLGen(b, queryShallow) }
func BenchmarkGraphQLGoNested(b *testing.B)  { benchGraphQLGo(b, queryNested) }
func BenchmarkGQLGenNested(b *testing.B)     { benchGQLGen(b, queryNested) }

func benchGraphQLGo(b *testing.B, query string) {
	e := newGraphQLGo(data.Dataset())
	_ = execGraphQLGo(e, query)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp := e.Execute(context.Background(), &graphql.Request{Query: query})
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		if len(resp.Data) == 0 {
			b.Fatal("empty data")
		}
		resp.Release()
	}
}

func benchGQLGen(b *testing.B, query string) {
	r := newGQLGen(data.Dataset())
	_ = r.execute(context.Background(), query)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp := r.run(ctx, query)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		if len(resp.Data) == 0 {
			b.Fatal("empty data")
		}
	}
}
