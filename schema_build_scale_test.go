package graphql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

type bsNode struct{ ID, Name string }

// bsSchema writes n object types and caps the Query root at qn fields, so the
// number of types and the width of the widest type can be varied apart.
func bsSchema(n, qn int) (string, []SchemaOption) {
	var sdl, q strings.Builder
	q.WriteString("type Query {\n")
	opts := make([]SchemaOption, 0, n+1)
	qf := make([]FieldOption, 0, qn)
	for i := range n {
		T := fmt.Sprintf("T%d", i)
		fmt.Fprintf(&sdl, "type %s { id: ID! name: String! count: Int! ok: Boolean! ref: T%d peers: [%s!]! }\n", T, (i+1)%n, T)
		opts = append(opts, Object[bsNode](T,
			Field("id", func(b *bsNode) ID { return ID(b.ID) }),
			Field("name", func(b *bsNode) string { return b.Name }),
			Field("count", func(*bsNode) int { return 0 }),
			Field("ok", func(*bsNode) bool { return true }),
			Field("ref", func(*bsNode) *bsNode { return nil }),
			Field("peers", func(*bsNode) []*bsNode { return nil }),
		))
		if i < qn {
			fmt.Fprintf(&q, "  q%d: %s\n", i, T)
			qf = append(qf, Field(fmt.Sprintf("q%d", i), func(Root) *bsNode { return nil }))
		}
	}
	q.WriteString("}\n")
	opts = append(opts, Query(qf...))
	return sdl.String() + q.String(), opts
}

func benchBuild(b *testing.B, n, qn int) {
	sdl, opts := bsSchema(n, qn)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewSchema(SDL(sdl), opts...); err != nil {
			b.Fatal(err)
		}
	}
}

func benchParse(b *testing.B, n, qn int) {
	sdl, _ := bsSchema(n, qn)
	src := []*ast.Source{{Name: "schema.graphql", Input: sdl}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := gqlparser.LoadSchema(src...); err != nil {
			b.Fatal(err)
		}
	}
}

// The variable in schema build cost is the width of the widest type, not the
// number of types. Measured at 4 800 types, changing only the Query root:
//
//	wide (4 800 Query fields)   77.9 ms   33.2 MB   602 241 allocs
//	narrow (100 Query fields)   34.1 ms   29.2 MB   536 373 allocs
//
// and across 1 600 to 4 800 types the wide curve multiplies by 4.96 where the
// narrow one multiplies by 3.38 for the same 3x in types. Roughly half of
// either is gqlparser's LoadSchema, whose FieldList.ForName is a linear scan,
// so validating a k-field type costs O(k^2); GC is not involved, which GOGC=off
// confirms by changing nothing.
//
// It matters because an ORM-generated schema puts one Query field per entity.
// The mitigation is schema design -- namespace the root rather than flatten it
// -- and these two benchmarks exist so the claim can be re-measured rather than
// believed.
func BenchmarkSchemaBuildWideRoot(b *testing.B)   { benchBuild(b, 4800, 4800) }
func BenchmarkSchemaBuildNarrowRoot(b *testing.B) { benchBuild(b, 4800, 100) }

// The parser's share of the same two, so the split is reproducible.
func BenchmarkSchemaParseWideRoot(b *testing.B)   { benchParse(b, 4800, 4800) }
func BenchmarkSchemaParseNarrowRoot(b *testing.B) { benchParse(b, 4800, 100) }
