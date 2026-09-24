// Package sdlprint renders a schema as SDL.
//
// It is gqlparser's formatter.FormatSchema, reduced to what PrintSDL uses and
// corrected where that formatter does not write SDL that reads back as the
// same schema:
//
//   - the schema description is written, and forces a schema definition even
//     when every root keeps its default name;
//   - a description is written as a block string only when one can hold it
//     unchanged, with """ escaped, and as a quoted string otherwise;
//   - a string value is quoted with GraphQL's escapes, not strconv.Quote's,
//     which include \x, \a, \v and \U that GraphQL does not accept.
//
// Everything else produces the formatter's output byte for byte
// (TestPrintMatchesFormatter), so a schema none of those touch prints as it
// always has. That test only compares what its corpus declares: when a
// gqlparser upgrade teaches the formatter a construct the AST could not hold
// before -- docs/upstream/0003 adds directives on directive definitions --
// add it here and to everyConstruct, or it prints without them.
//
// Derived from github.com/vektah/gqlparser/v2/formatter:
//
//	Copyright (c) 2018 Adam Scarr
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE.
package sdlprint

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/vektah/gqlparser/v2/ast"
)

// Print renders s as SDL, excluding built-in types and directives.
func Print(s *ast.Schema) string {
	p := &printer{}
	p.schema(s)
	return p.sb.String()
}

type printer struct {
	sb       strings.Builder
	indent   int
	padNext  bool
	lineHead bool
}

func (p *printer) writeIndent() {
	if p.lineHead {
		p.sb.WriteString(strings.Repeat("\t", p.indent))
	}
	p.lineHead = false
	p.padNext = false
}

func (p *printer) newline() *printer {
	p.sb.WriteString("\n")
	p.lineHead = true
	p.padNext = false
	return p
}

func (p *printer) word(w string) *printer {
	if p.lineHead {
		p.writeIndent()
	}
	if p.padNext {
		p.sb.WriteString(" ")
	}
	p.sb.WriteString(strings.TrimSpace(w))
	p.padNext = true
	return p
}

func (p *printer) str(s string) *printer {
	if p.lineHead {
		p.writeIndent()
	}
	if p.padNext {
		p.sb.WriteString(" ")
	}
	p.sb.WriteString(s)
	p.padNext = false
	return p
}

func (p *printer) noPadding() *printer {
	p.padNext = false
	return p
}

func (p *printer) needPadding() *printer {
	p.padNext = true
	return p
}

func (p *printer) description(s string) {
	if s == "" {
		return
	}
	if !blockSafe(s) {
		p.str(quote(s)).newline()
		return
	}
	p.str(`"""`).newline()
	for line := range strings.SplitSeq(strings.ReplaceAll(s, `"""`, `\"""`), "\n") {
		p.str(line).newline()
	}
	p.str(`"""`).newline()
}

// blockSafe reports whether s reads back unchanged from the block string
// description writes. That block starts its content on the line after the
// opening quotes, so every line takes part in removing the common
// indentation; a block string also drops leading and trailing blank lines,
// normalizes line terminators and cannot hold a control character.
func blockSafe(s string) bool {
	lines := strings.Split(s, "\n")
	if blank(lines[0]) || blank(lines[len(lines)-1]) {
		return false
	}
	commonIndent := true
	for _, line := range lines {
		for _, r := range line {
			if r < ' ' && r != '\t' || r == 0x7f {
				return false
			}
		}
		if !blank(line) && line[0] != ' ' && line[0] != '\t' {
			commonIndent = false
		}
	}
	return !commonIndent
}

func blank(line string) bool { return strings.Trim(line, " \t") == "" }

// quote writes s as a GraphQL string value. It escapes what strconv.Quote
// escapes, so the formatter's output is unchanged wherever that was already
// valid, but only ever with the escapes GraphQL defines. A non-printable rune
// outside the Basic Multilingual Plane is written as itself: GraphQL's only
// escape for one is a surrogate pair, and the rune is a valid source character.
func quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if !unicode.IsPrint(r) && r <= 0xffff {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func (p *printer) schema(s *ast.Schema) {
	inSchema := false
	start := func() {
		if inSchema {
			return
		}
		inSchema = true
		p.description(s.Description)
		p.word("schema")
		p.directives(s.SchemaDirectives)
		p.str("{").newline()
		p.indent++
	}

	need := s.Query != nil && s.Query.Name != "Query" ||
		s.Mutation != nil && s.Mutation.Name != "Mutation" ||
		s.Subscription != nil && s.Subscription.Name != "Subscription" ||
		s.Description != ""
	for _, root := range []struct {
		op  string
		def *ast.Definition
	}{{"query", s.Query}, {"mutation", s.Mutation}, {"subscription", s.Subscription}} {
		if need && root.def != nil {
			start()
			p.word(root.op).noPadding().str(":").needPadding()
			p.word(root.def.Name).newline()
		}
	}
	if inSchema {
		p.indent--
		p.str("}").newline()
	} else if len(s.SchemaDirectives) > 0 {
		p.word("extend").word("schema")
		p.directives(s.SchemaDirectives)
		p.newline()
	}

	for _, name := range sortedKeys(s.Directives) {
		p.directiveDefinition(s.Directives[name])
	}
	for _, name := range sortedKeys(s.Types) {
		p.definition(s.Types[name])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func builtIn(pos *ast.Position) bool { return pos != nil && pos.Src != nil && pos.Src.BuiltIn }

func (p *printer) directiveDefinition(d *ast.DirectiveDefinition) {
	if builtIn(d.Position) {
		return
	}
	p.description(d.Description)
	p.word("directive").str("@").word(d.Name)
	if len(d.Arguments) != 0 {
		p.noPadding()
		p.argumentDefinitions(d.Arguments)
	}
	if d.IsRepeatable {
		p.word("repeatable")
	}
	if len(d.Locations) != 0 {
		p.word("on")
		for i, loc := range d.Locations {
			p.word(string(loc))
			if i != len(d.Locations)-1 {
				p.word("|")
			}
		}
	}
	p.newline()
}

func (p *printer) definition(d *ast.Definition) {
	if strings.HasPrefix(d.Name, "__") || d.BuiltIn {
		return
	}
	p.description(d.Description)
	switch d.Kind {
	case ast.Scalar:
		p.word("scalar").word(d.Name)
	case ast.Object:
		p.word("type").word(d.Name)
	case ast.Interface:
		p.word("interface").word(d.Name)
	case ast.Union:
		p.word("union").word(d.Name)
	case ast.Enum:
		p.word("enum").word(d.Name)
	case ast.InputObject:
		p.word("input").word(d.Name)
	}
	if len(d.Interfaces) != 0 {
		p.word("implements").word(strings.Join(d.Interfaces, " & "))
	}
	p.directives(d.Directives)
	if len(d.Types) != 0 {
		p.word("=").word(strings.Join(d.Types, " | "))
	}
	if len(d.Fields) != 0 {
		p.str("{").newline()
		p.indent++
		for _, f := range d.Fields {
			p.field(f)
		}
		p.indent--
		p.str("}")
	}
	if len(d.EnumValues) != 0 {
		p.str("{").newline()
		p.indent++
		for _, v := range d.EnumValues {
			p.description(v.Description)
			p.word(v.Name)
			p.directives(v.Directives)
			p.newline()
		}
		p.indent--
		p.str("}")
	}
	p.newline()
}

func (p *printer) field(f *ast.FieldDefinition) {
	if strings.HasPrefix(f.Name, "__") || builtIn(f.Position) {
		return
	}
	p.description(f.Description)
	p.word(f.Name).noPadding()
	p.argumentDefinitions(f.Arguments)
	p.noPadding().str(":").needPadding()
	p.word(f.Type.String())
	if f.DefaultValue != nil {
		p.word("=")
		p.str(Value(f.DefaultValue))
	}
	p.directives(f.Directives)
	p.newline()
}

func (p *printer) argumentDefinitions(args ast.ArgumentDefinitionList) {
	if len(args) == 0 {
		return
	}
	p.str("(")
	for i, a := range args {
		p.argumentDefinition(a)
		// The formatter writes no comma after the last argument or after one
		// that ended on a new line of its own.
		if i != len(args)-1 && a.Description == "" {
			p.noPadding().word(",")
		}
	}
	p.noPadding().str(")").needPadding()
}

func (p *printer) argumentDefinition(a *ast.ArgumentDefinition) {
	if a.Description != "" {
		p.newline()
		p.indent++
		p.description(a.Description)
	}
	p.word(a.Name).noPadding().str(":").needPadding()
	p.word(a.Type.String())
	if a.DefaultValue != nil {
		p.word("=")
		p.str(Value(a.DefaultValue))
	}
	p.needPadding().directives(a.Directives)
	if a.Description != "" {
		p.indent--
		p.newline()
	}
}

func (p *printer) directives(list ast.DirectiveList) {
	for _, d := range list {
		p.str("@").word(d.Name)
		if len(d.Arguments) == 0 {
			continue
		}
		p.noPadding().str("(")
		for i, a := range d.Arguments {
			p.word(a.Name).noPadding().str(":").needPadding()
			p.str(Value(a.Value))
			if i != len(d.Arguments)-1 {
				p.noPadding().word(",")
			}
		}
		p.str(")").needPadding()
	}
}

// Value renders v as a GraphQL literal: ast.Value.String with quote in place
// of strconv.Quote. Introspection reports a defaultValue with it too, since a
// client parses that string as a literal.
func Value(v *ast.Value) string {
	switch v.Kind {
	case ast.StringValue, ast.BlockValue:
		return quote(v.Raw)
	case ast.ListValue:
		items := make([]string, len(v.Children))
		for i, c := range v.Children {
			items[i] = Value(c.Value)
		}
		return "[" + strings.Join(items, ",") + "]"
	case ast.ObjectValue:
		items := make([]string, len(v.Children))
		for i, c := range v.Children {
			items[i] = c.Name + ":" + Value(c.Value)
		}
		return "{" + strings.Join(items, ",") + "}"
	default:
		return v.String()
	}
}
