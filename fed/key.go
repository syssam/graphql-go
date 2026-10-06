package fed

import (
	"errors"
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/syssam/graphql-go/internal/fedlink"
)

// checkKeys reports a @key that selects a field its type does not have, or
// that is written in a way a field set may not be.
//
// The first composes into a router that fetches nothing from this subgraph
// and the second does not compose at all. The SDL says so at start-up, which
// is when it should be said rather than at the first fetch that quietly
// returns null, or in the pipeline that runs composition.
func checkKeys(doc *ast.SchemaDocument, names fedlink.Spellings) error {
	types := map[string]*ast.Definition{}
	index := func(defs ast.DefinitionList) {
		for _, def := range defs {
			if prev, ok := types[def.Name]; ok {
				// An extension adds to what the base declared, and a key may
				// select a field from either.
				prev.Fields = append(prev.Fields, def.Fields...)
				continue
			}
			cp := *def
			cp.Fields = append(ast.FieldList(nil), def.Fields...)
			types[def.Name] = &cp
		}
	}
	index(doc.Definitions)
	index(doc.Extensions)

	for _, defs := range []ast.DefinitionList{doc.Definitions, doc.Extensions} {
		for _, def := range defs {
			for _, d := range def.Directives {
				if !names.Is(d.Name, "key") {
					continue
				}
				arg := d.Arguments.ForName("fields")
				if arg == nil || arg.Value == nil || (arg.Value.Kind != ast.StringValue && arg.Value.Kind != ast.BlockValue) {
					return fmt.Errorf("fed: @key on %s has no fields argument", def.Name)
				}
				sels, err := parseFieldSet(arg.Value.Raw)
				if err != nil {
					return fmt.Errorf("fed: @key(fields: %q) on %s: %w", arg.Value.Raw, def.Name, err)
				}
				if err := checkFieldSet(types, def.Name, def.Name, sels); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// parseFieldSet reads the selection syntax a field set is written in. It is a
// selection set without a type condition, so it parses as the body of an
// anonymous query.
func parseFieldSet(fields string) (ast.SelectionSet, error) {
	doc, err := parser.ParseQuery(&ast.Source{Name: "fieldset", Input: "{" + fields + "}"})
	if err != nil {
		return nil, err
	}
	if len(doc.Operations) != 1 {
		return nil, errors.New("not a field set")
	}
	return doc.Operations[0].SelectionSet, nil
}

// checkFieldSet checks the selections a key on owner makes into typeName,
// which is owner itself at the top and the type of a selected field below it.
func checkFieldSet(types map[string]*ast.Definition, owner, typeName string, sels ast.SelectionSet) error {
	def, ok := types[typeName]
	if !ok {
		return fmt.Errorf("fed: @key selects into %s, which this subgraph does not declare", typeName)
	}
	for _, sel := range sels {
		f, ok := sel.(*ast.Field)
		if !ok {
			return fmt.Errorf("fed: @key on %s uses a fragment, which a field set may not contain", owner)
		}
		fd := def.Fields.ForName(f.Name)
		if fd == nil {
			return fmt.Errorf("fed: @key selects %s.%s, and %s has no field %q", typeName, f.Name, typeName, f.Name)
		}
		// A field set parses as a selection, so it parses with everything a
		// selection may carry. A key names fields and nothing else: the
		// router sends them back under their own names, with no arguments
		// to send and no directive to apply.
		switch {
		case f.Alias != "" && f.Alias != f.Name:
			return fmt.Errorf("fed: @key on %s gives %s.%s the alias %q, which a field set may not contain", owner, typeName, f.Name, f.Alias)
		case len(f.Arguments) > 0:
			return fmt.Errorf("fed: @key on %s passes an argument to %s.%s, which a field set may not contain", owner, typeName, f.Name)
		case len(f.Directives) > 0:
			return fmt.Errorf("fed: @key on %s applies @%s to %s.%s, which a field set may not contain", owner, f.Directives[0].Name, typeName, f.Name)
		}
		if len(f.SelectionSet) > 0 {
			if err := checkFieldSet(types, owner, fd.Type.Name(), f.SelectionSet); err != nil {
				return err
			}
			continue
		}
		if inner, ok := types[fd.Type.Name()]; ok && inner.IsCompositeType() {
			return fmt.Errorf("fed: @key on %s selects %s.%s, of type %s, without saying which of its fields make the key: write \"%s { ... }\"",
				owner, typeName, f.Name, inner.Name, f.Name)
		}
	}
	return nil
}
