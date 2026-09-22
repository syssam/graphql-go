package graphql

import (
	"context"
	"testing"
)

const bsIntrospection = `{
  __schema {
    queryType { name }
    types {
      kind name
      fields(includeDeprecated: true) {
        name
        args { name type { kind name ofType { kind name } } }
        type { kind name ofType { kind name ofType { kind name } } }
      }
      inputFields { name type { kind name ofType { kind name } } }
      interfaces { kind name }
      enumValues(includeDeprecated: true) { name }
      possibleTypes { kind name }
    }
  }
}`

func introspectN(tb testing.TB, n int) (*Executor, int) {
	tb.Helper()
	sdl, opts := bsSchema(n, 100)
	s, err := NewSchema(SDL(sdl), opts...)
	if err != nil {
		tb.Fatal(err)
	}
	e := NewExecutor(s)
	resp := e.Execute(context.Background(), &Request{Query: bsIntrospection})
	if len(resp.Errors) > 0 {
		tb.Fatalf("introspection: %v", resp.Errors[0])
	}
	return e, len(resp.Data)
}

func TestIntrospectionScale(t *testing.T) {
	for _, n := range []int{1600, 3600, 4200, 4800} {
		_, size := introspectN(t, n)
		t.Logf("n=%-5d response %8.2f MB", n, float64(size)/(1<<20))
	}
}

func benchIntrospect(b *testing.B, n int) {
	e, _ := introspectN(b, n)
	req := &Request{Query: bsIntrospection}
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

// Full introspection is the most expensive query either engine will serve and
// is usually reachable without authentication: GraphiQL, Apollo Studio, schema
// registries and codegen tools all send it on connect.
//
// Execution is linear in the schema -- 3.7x the time for 3x the types -- but
// the bytes are not, and the reason is a cliff rather than a curve. A buffer
// whose capacity passes maxPooledCap (4 MiB) is never returned to the pool, so
// the next request rebuilds it from 512 bytes. append overshoots, so a 3.3 MB
// response already reaches a 4.12 MB capacity:
//
//	3 600 types   2.83 MB response   16.6 MB/op   pooled
//	4 200 types   3.30 MB response   38.4 MB/op   not pooled
//	4 800 types   3.77 MB response   40.9 MB/op   not pooled
//
// Raising maxPooledCap to 64 MiB takes those to 19.4 and 21.9 MB/op, which is
// what says the cliff is the cause and not the response size itself. The two
// benchmarks straddle it deliberately; see docs/performance.md.
func BenchmarkIntrospectPooled(b *testing.B)   { benchIntrospect(b, 3600) }
func BenchmarkIntrospectOverPool(b *testing.B) { benchIntrospect(b, 4800) }
