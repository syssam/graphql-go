package codegen

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ident is the Go identifier for an SDL field or argument name: Config.FieldNames when it
// names one, the derived name otherwise.
func (b *builder) ident(sdlName string) string {
	if g, ok := b.cfg.FieldNames[sdlName]; ok {
		return g
	}
	return goIdent(sdlName)
}

// fieldIdent is ident for a field or input field of a type. A FieldNames key
// written as the schema coordinate, Type.field, names that one field and
// outranks a bare key, which names it on every type.
func (b *builder) fieldIdent(typeName, field string) string {
	if g, ok := b.cfg.FieldNames[typeName+"."+field]; ok {
		return g
	}
	return b.ident(field)
}

// argIdent is ident for one argument, whose coordinate is Type.field(arg:).
func (b *builder) argIdent(typeName, field, arg string) string {
	if g, ok := b.cfg.FieldNames[typeName+"."+field+"("+arg+":)"]; ok {
		return g
	}
	return b.ident(arg)
}

// checkFieldNames refuses a Config.FieldNames value that is not an exported Go identifier,
// since the field it names would be unexported and the schema would not bind.
//
// It also refuses a key that names no field or argument in the schema: that is a typo,
// and every other configuration key is checked the same way. Keys are visited in order,
// so the same configuration reports the same thing.
func (b *builder) checkFieldNames() error {
	if len(b.cfg.FieldNames) == 0 {
		return nil
	}
	known := map[string]bool{}
	for name, def := range b.schema.Types {
		for _, fd := range def.Fields {
			known[fd.Name], known[name+"."+fd.Name] = true, true
			for _, arg := range fd.Arguments {
				known[arg.Name], known[name+"."+fd.Name+"("+arg.Name+":)"] = true, true
			}
		}
	}
	var unknown []string
	for _, sdl := range slices.Sorted(maps.Keys(b.cfg.FieldNames)) {
		if g := b.cfg.FieldNames[sdl]; !token.IsIdentifier(g) || !token.IsExported(g) {
			return fmt.Errorf("FieldNames[%q] = %q is not an exported Go identifier", sdl, g)
		}
		if !known[sdl] {
			unknown = append(unknown, strconv.Quote(sdl))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("FieldNames names no field or argument in the schema: %s", strings.Join(unknown, ", "))
	}
	return nil
}

func goIdent(name string) string {
	if name == "id" {
		return "ID"
	}
	// A leading underscore has no upper case, so upper-casing the first rune
	// left `_lastUpdatedAt` unexported -- and Input[T] skips unexported
	// fields, so the input could never bind and NewSchema refused the whole
	// schema. The generated Go compiled perfectly; only building the schema
	// found it, 342 times on one real schema. A leading underscore is legal
	// in SDL and conventional for a meta field, so it has to produce an
	// exported name rather than be rejected.
	//
	// The X stays rather than being dropped, so that `_x` and `x` on one type
	// do not both become X and collide -- the `graphql` tag pins the SDL name,
	// so a collision would be a compile error in generated code.
	trimmed := strings.TrimLeft(name, "_")
	if trimmed == "" {
		return "X"
	}
	r, w := utf8.DecodeRuneInString(trimmed)
	s := string(unicode.ToUpper(r)) + trimmed[w:]
	if strings.HasSuffix(s, "Id") {
		s = s[:len(s)-2] + "ID"
	}
	// Prefix when the name was underscored, or when upper-casing cannot
	// export it at all -- a digit or a symbol first.
	if trimmed != name || !unicode.IsUpper(unicode.ToUpper(r)) {
		s = "X" + s
	}
	return s
}

func (b *builder) argsName(typeName, field string) string {
	if b.isRoot(typeName) {
		return b.fieldIdent(typeName, field) + "Args"
	}
	return typeName + b.fieldIdent(typeName, field) + "Args"
}

func (b *builder) resolverMethod(typeName, field string) string {
	if b.isRoot(typeName) {
		return b.fieldIdent(typeName, field)
	}
	return typeName + b.fieldIdent(typeName, field)
}
