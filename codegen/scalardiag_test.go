package codegen

import (
	"fmt"
	"strings"
	"testing"
)

// gqlc emits a Go type for an unmapped custom scalar but cannot know its wire
// format, so the author must supply graphql.Scalar at NewSchema. The generator
// knows this while it is generating and used to say nothing: on one real
// schema the silence cost 21 641 errors at build time, for eight scalars.
// Saying it at generate time is the same information, hours earlier.
func TestUnboundScalarsAreReportedWhileGenerating(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
scalar Money
scalar Stamp
scalar Mapped
type Doc { id: ID! price: Money! at: Stamp! m: Mapped! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct{ ID string }
`
	dir := writeEntModule(t, sdl, source)
	var notes []string
	autoGenerate(t, dir, Config{
		// Mapped is reported too, and that is the point: models: chooses the
		// Go type, it does not register a marshaller. Believing otherwise is
		// what made 21 641 of one real schema's errors a surprise.
		Models: map[string]string{"Doc": "hello/ent.Doc", "Mapped": "time.Time"},
		Notef:  func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) },
	})

	var scalarNote string
	for _, n := range notes {
		if strings.Contains(n, "graphql.Scalar") {
			scalarNote = n
		}
	}
	if scalarNote == "" {
		t.Fatalf("no scalar diagnostic was reported: %v", notes)
	}
	for _, want := range []string{"Money", "Stamp", "Mapped"} {
		if !strings.Contains(scalarNote, want) {
			t.Errorf("the diagnostic does not name %s: %s", want, scalarNote)
		}
	}
	// The Go type is named, so the fix can be pasted rather than worked out.
	if !strings.Contains(scalarNote, "time.Time") {
		t.Errorf("the diagnostic does not name the mapped Go type: %s", scalarNote)
	}
	// time.Time has a binding in the engine, so its line is that call, and
	// only its line: Stamp is a Time by name alone.
	if !strings.Contains(scalarNote, `Mapped (time.Time: graphql.Time("Mapped"))`) || strings.Count(scalarNote, "graphql.Time(") != 1 {
		t.Errorf("the diagnostic does not offer graphql.Time for the time.Time scalar alone: %s", scalarNote)
	}
	// The built-in scalars are never the author's problem.
	for _, never := range []string{"String", "Int", "Boolean", "ID", "Float"} {
		if strings.Contains(scalarNote, never) {
			t.Errorf("a built-in scalar was reported: %s", scalarNote)
		}
	}
}

// Silence when there is nothing to say: a schema whose scalars are all bound
// must produce no diagnostic at all, or the signal is worth nothing.
func TestNoScalarDiagnosticWhenAllAreBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
type Doc { id: ID! name: String! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct {
	ID   string
	Name string
}
`
	dir := writeEntModule(t, sdl, source)
	var notes []string
	autoGenerate(t, dir, Config{Notef: func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) }})
	for _, n := range notes {
		if strings.Contains(n, "graphql.Scalar") {
			t.Errorf("a scalar diagnostic was reported for a schema with none: %s", n)
		}
	}
}
