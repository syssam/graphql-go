package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// wideAuthzSchema builds a schema whose Query has n fields, each declaring a
// requirement, so a selection of all of them produces n AuthSites. The small
// fixture used by BenchmarkExecuteWithAuthorizer has two, which is too few for
// the per-site cost of the Authorizer's walk to show at all.
func wideAuthzSchema(tb testing.TB, n int) (*Schema, string) {
	tb.Helper()
	var sdl, sel strings.Builder
	sdl.WriteString("directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT\ntype Query {\n")
	for i := range n {
		fmt.Fprintf(&sdl, "  f%d: String! @requiresScopes(scopes: [[\"read\"]])\n", i)
		fmt.Fprintf(&sel, " f%d", i)
	}
	sdl.WriteString("}\n")

	fields := make([]FieldOption, 0, n)
	for i := range n {
		fields = append(fields, Field(fmt.Sprintf("f%d", i), func(Root) string { return "v" }))
	}
	s, err := NewSchema(SDL(sdl.String()), Query(fields...))
	if err != nil {
		tb.Fatalf("NewSchema: %v", err)
	}
	return s, "{" + sel.String() + " }"
}

func benchWideAuthz(b *testing.B, n int) {
	s, q := wideAuthzSchema(b, n)
	held := map[string]bool{"read": true}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return held })))
	benchRun(b, e, q)
}

func BenchmarkAuthorizerWide16(b *testing.B)  { benchWideAuthz(b, 16) }
func BenchmarkAuthorizerWide64(b *testing.B)  { benchWideAuthz(b, 64) }
func BenchmarkAuthorizerWide256(b *testing.B) { benchWideAuthz(b, 256) }

// TestWideAuthzSchemaHasASitePerField keeps the benchmark honest: if the shape
// ever stopped recording a site per field, the benchmark would quietly measure
// nothing and still report a number.
func TestWideAuthzSchemaHasASitePerField(t *testing.T) {
	const n = 16
	s, q := wideAuthzSchema(t, n)
	e := NewExecutor(s)
	p := planFor(t, e, q)
	if p.shape == nil {
		t.Fatal("no shape")
	}
	if got := len(p.shape.sites); got != n {
		t.Fatalf("sites = %d, want %d", got, n)
	}
}
