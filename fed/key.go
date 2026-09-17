package fed

import (
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// checkKeys reports a @key that selects a field its type does not have.
//
// A key like that composes into a router that fetches nothing from this
// subgraph, and the SDL says so at start-up, which is when it should be said
// rather than at the first fetch that quietly returns null.
func checkKeys(doc *ast.SchemaDocument) error {
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
				if d.Name != "key" {
					continue
				}
				arg := d.Arguments.ForName("fields")
				if arg == nil || arg.Value == nil || arg.Value.Kind != ast.StringValue {
					return fmt.Errorf("fed: @key on %s has no fields argument", def.Name)
				}
				sels, err := parseFieldSet(arg.Value.Raw)
				if err != nil {
					return fmt.Errorf("fed: @key(fields: %q) on %s: %w", arg.Value.Raw, def.Name, err)
				}
				if err := checkFieldSet(types, def.Name, sels); err != nil {
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
		return nil, fmt.Errorf("not a field set")
	}
	return doc.Operations[0].SelectionSet, nil
}

func checkFieldSet(types map[string]*ast.Definition, typeName string, sels ast.SelectionSet) error {
	def, ok := types[typeName]
	if !ok {
		return fmt.Errorf("fed: @key selects into %s, which this subgraph does not declare", typeName)
	}
	for _, sel := range sels {
		f, ok := sel.(*ast.Field)
		if !ok {
			return fmt.Errorf("fed: @key on %s uses a fragment, which a field set may not contain", typeName)
		}
		fd := def.Fields.ForName(f.Name)
		if fd == nil {
			return fmt.Errorf("fed: @key selects %s.%s, and %s has no field %q", typeName, f.Name, typeName, f.Name)
		}
		if len(f.SelectionSet) > 0 {
			if err := checkFieldSet(types, fd.Type.Name(), f.SelectionSet); err != nil {
				return err
			}
		}
	}
	return nil
}
