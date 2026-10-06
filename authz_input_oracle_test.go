package graphql

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// The walk below is the one inputKeys used until it was rewritten to share
// path storage: it cloned the whole path for every key and joined it into a
// string to deduplicate, which is quadratic in nesting depth. It is kept as
// the oracle that pins the replacement, since what an Authorizer is shown --
// which keys, in which order, with which paths -- is the contract and the
// rewrite must not move any of it. Do not reintroduce it into the production
// path.

func inputKeysOracle(types map[string]*ast.Definition, t *ast.Type, v *ast.Value, vars map[string]any) []InputKey {
	w := inputWalkOracle{types: types, vars: vars, seen: map[string]bool{}}
	w.ast(t, v, nil)
	return w.keys
}

type inputWalkOracle struct {
	types map[string]*ast.Definition
	vars  map[string]any
	keys  []InputKey
	seen  map[string]bool
}

func (w *inputWalkOracle) add(path []string, enum string, null bool) {
	id := strings.Join(path, "\x00") + "\x01" + enum
	if null {
		id += "\x02"
	}
	if w.seen[id] {
		return
	}
	w.seen[id] = true
	w.keys = append(w.keys, InputKey{Path: slices.Clone(path), Enum: enum, Null: null})
}

func (w *inputWalkOracle) isComposite(v *ast.Value) bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case ast.ObjectValue, ast.ListValue:
		return true
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return false
		}
		switch raw.(type) {
		case map[string]any, []any:
			return true
		}
	}
	return false
}

func (w *inputWalkOracle) ast(t *ast.Type, v *ast.Value, path []string) {
	if v == nil {
		return
	}
	switch v.Kind {
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return
		}
		w.raw(t, raw, path)
	case ast.NullValue:
		w.add(path, "", true)
	case ast.ListValue:
		elem := t
		if t.Elem != nil {
			elem = t.Elem
		}
		for _, c := range v.Children {
			if c.Value != nil && c.Value.Kind == ast.NullValue {
				continue
			}
			w.ast(elem, c.Value, path)
		}
	case ast.ObjectValue:
		def := w.types[t.Name()]
		if def == nil {
			return
		}
		for _, c := range v.Children {
			fd := def.Fields.ForName(c.Name)
			if fd == nil {
				continue
			}
			child := append(slices.Clone(path), c.Name)
			if w.isComposite(c.Value) {
				w.add(child, "", false)
			}
			w.ast(fd.Type, c.Value, child)
		}
	case ast.EnumValue:
		if def := w.types[t.Name()]; def != nil && def.Kind == ast.Enum {
			w.add(path, v.Raw, false)
		} else if len(path) > 0 {
			w.add(path, "", false)
		}
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}

func (w *inputWalkOracle) raw(t *ast.Type, v any, path []string) {
	if v == nil {
		w.add(path, "", true)
		return
	}
	if t.Elem != nil {
		if list, ok := v.([]any); ok {
			for _, e := range list {
				if e == nil {
					continue
				}
				w.raw(t.Elem, e, path)
			}
			return
		}
		w.raw(t.Elem, v, path)
		return
	}
	def := w.types[t.Name()]
	if def == nil {
		return
	}
	switch def.Kind {
	case ast.Enum:
		if s, ok := v.(string); ok && def.EnumValues.ForName(s) != nil {
			w.add(path, s, false)
		}
	case ast.InputObject:
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for _, name := range slices.Sorted(maps.Keys(m)) {
			fd := def.Fields.ForName(name)
			if fd == nil {
				continue
			}
			fv := m[name]
			child := append(slices.Clone(path), name)
			switch fv.(type) {
			case map[string]any, []any:
				w.add(child, "", false)
			}
			w.raw(fd.Type, fv, child)
		}
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}

// inputGen builds argument values no validator has seen, as a literal and as
// what a variable decodes to, with one nested in the other. The walk is
// written to tolerate input that coercion will go on to refuse -- it runs on
// whatever the request carried -- so the generator is deliberately looser than
// the schema: unknown keys, a scalar where an object belongs, an undeclared
// enum value, a single value standing in for a list.
type inputGen struct {
	r    *rand.Rand
	vars map[string]any
}

var inputGenFields = []string{"not", "and", "nameContains", "taxNumber", "meta", "metaTags", "order", "orders", "group", "bogus"}

var inputGenEnums = []string{"NAME", "TAX_NUMBER", "ASC", "BOGUS"}

// raw is a value as a variable carries it.
func (g *inputGen) raw(depth int) any {
	switch n := g.r.IntN(12); {
	case n == 0:
		return nil
	case n == 1:
		return "text"
	case n == 2:
		return inputGenEnums[g.r.IntN(len(inputGenEnums))]
	case n == 3:
		return float64(g.r.IntN(9))
	case n <= 5 && depth > 0:
		list := make([]any, g.r.IntN(4))
		for i := range list {
			list[i] = g.raw(depth - 1)
		}
		return list
	case depth > 0:
		m := make(map[string]any)
		for range g.r.IntN(5) {
			m[inputGenFields[g.r.IntN(len(inputGenFields))]] = g.raw(depth - 1)
		}
		return m
	}
	return true
}

// literal is a value as the document spells it, which may hand off to a
// variable at any position.
func (g *inputGen) literal(depth int) *ast.Value {
	switch n := g.r.IntN(14); {
	case n == 0:
		return &ast.Value{Kind: ast.NullValue, Raw: "null"}
	case n == 1:
		return &ast.Value{Kind: ast.StringValue, Raw: "text"}
	case n == 2:
		return &ast.Value{Kind: ast.EnumValue, Raw: inputGenEnums[g.r.IntN(len(inputGenEnums))]}
	case n == 3:
		return &ast.Value{Kind: ast.IntValue, Raw: "7"}
	case n == 4:
		name := fmt.Sprintf("v%d", len(g.vars))
		// One in four is declared and not supplied, which reports nothing.
		if g.r.IntN(4) > 0 {
			g.vars[name] = g.raw(depth)
		} else {
			g.vars[name+"absent"] = nil
		}
		return &ast.Value{Kind: ast.Variable, Raw: name}
	case n <= 7 && depth > 0:
		v := &ast.Value{Kind: ast.ListValue}
		for range g.r.IntN(4) {
			v.Children = append(v.Children, &ast.ChildValue{Value: g.literal(depth - 1)})
		}
		return v
	case depth > 0:
		return g.object(depth)
	}
	return &ast.Value{Kind: ast.BooleanValue, Raw: "true"}
}

func (g *inputGen) object(depth int) *ast.Value {
	v := &ast.Value{Kind: ast.ObjectValue}
	for range 1 + g.r.IntN(5) {
		v.Children = append(v.Children, &ast.ChildValue{
			Name:  inputGenFields[g.r.IntN(len(inputGenFields))],
			Value: g.literal(depth - 1),
		})
	}
	return v
}

// argument is what a client usually sends for one: an object, spelled out or
// through a variable. The rest of the time it is anything at all.
func (g *inputGen) argument(depth int) *ast.Value {
	switch g.r.IntN(5) {
	case 0:
		return g.literal(depth)
	case 1:
		g.vars["arg"] = map[string]any{"not": g.raw(depth), "and": g.raw(depth), "order": g.raw(depth)}
		return &ast.Value{Kind: ast.Variable, Raw: "arg"}
	}
	return g.object(depth)
}

const inputOracleSDL = `
scalar JSON
enum OrderField { NAME TAX_NUMBER }
enum Direction { ASC DESC }
input Order { field: OrderField! direction: Direction = ASC }
input Where {
  not: Where
  and: [Where]
  nameContains: String
  taxNumber: String
  meta: JSON
  metaTags: [JSON]
  order: Order
  orders: [[Order!]]
  group: [OrderField!]
}
type Query { q(where: Where, wheres: [Where!], group: [OrderField!], field: OrderField, meta: JSON): String }
`

// What an Authorizer is shown for an argument is the contract: the same keys,
// in the same order, each with the same path, and two positions counted as one
// exactly when they were before. DeepEqual holds all four at once, a nil path
// against an empty one included.
func TestInputKeysMatchTheWalkTheyReplaced(t *testing.T) {
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: inputOracleSDL})
	args := schema.Types["Query"].Fields.ForName("q").Arguments

	check := func(t *testing.T, typ *ast.Type, v *ast.Value, vars map[string]any) int {
		t.Helper()
		want := inputKeysOracle(schema.Types, typ, v, vars)
		budget := maxInputPathElems
		got, ok := inputKeys(schema.Types, typ, v, vars, &budget)
		if !ok {
			t.Fatalf("the walk refused an input the generator keeps small")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("type %s\n got  %v\n want %v", typ.String(), got, want)
		}
		return len(got)
	}

	keys, nonEmpty := 0, 0
	for seed := range uint64(4000) {
		g := &inputGen{r: rand.New(rand.NewPCG(seed, 1)), vars: map[string]any{}}
		// Mostly the two input-object arguments, where there are keys to
		// report; the enum and scalar ones are the short tail.
		typ := args[g.r.IntN(2)].Type
		if g.r.IntN(5) == 0 {
			typ = args[g.r.IntN(len(args))].Type
		}
		v := g.argument(2 + g.r.IntN(5))
		n := check(t, typ, v, g.vars)
		keys += n
		if n > 0 {
			nonEmpty++
		}
	}
	// A generator that produced nothing reportable would agree with any walk.
	if nonEmpty < 2000 || keys < 10000 {
		t.Fatalf("only %d of 4000 inputs reported anything (%d keys); the corpus no longer exercises the walk", nonEmpty, keys)
	}

	// The shapes the rewrite is about: a chain, where every path is a prefix
	// of the next, and a comb, where every level also carries a leaf of its
	// own and so a path no other shares its last element with.
	chain := any(map[string]any{"nameContains": "x"})
	comb := any(map[string]any{"nameContains": "x"})
	for range 200 {
		chain = map[string]any{"not": chain}
		comb = map[string]any{"not": comb, "taxNumber": "y", "and": []any{map[string]any{"meta": "z"}}}
	}
	where := args.ForName("where").Type
	variable := &ast.Value{Kind: ast.Variable, Raw: "w"}
	check(t, where, variable, map[string]any{"w": chain})
	check(t, where, variable, map[string]any{"w": comb})
}

// Sharing storage between paths must not let one key's path change another's:
// an Authorizer is free to append to a path it was handed.
func TestInputKeyPathsDoNotAliasOnAppend(t *testing.T) {
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: inputOracleSDL})
	where := schema.Types["Query"].Fields.ForName("q").Arguments.ForName("where").Type
	vars := map[string]any{"w": map[string]any{"not": map[string]any{"not": map[string]any{"nameContains": "x", "taxNumber": "y"}}}}
	v := &ast.Value{Kind: ast.Variable, Raw: "w"}
	budget := maxInputPathElems
	keys, _ := inputKeys(schema.Types, where, v, vars, &budget)
	before := fmt.Sprint(keys)
	for _, k := range keys {
		_ = append(k.Path, "appended")
	}
	if after := fmt.Sprint(keys); after != before {
		t.Fatalf("appending to one key's path rewrote another's:\n before %s\n after  %s", before, after)
	}
}
