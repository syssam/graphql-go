package codegen

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/vektah/gqlparser/v2/ast"
)

// checkTagDirectives refuses a tag directive the struct-tag syntax cannot carry. The
// generated field would otherwise be a tag that does not parse (reflect drops the whole
// tag silently) or one that means something else, so the run fails instead.
func (b *builder) checkTagDirectives() error {
	if b.cfg.TagDirective.IsZero() {
		return nil
	}
	for _, name := range b.typeNames(ast.InputObject) {
		for _, fd := range b.schema.Types[name].Fields {
			if _, err := b.tagsFor(fd); err != nil {
				return fmt.Errorf("%s.%s: %w", name, fd.Name, err)
			}
		}
	}
	return nil
}

func isStringValue(v *ast.Value) bool {
	return v != nil && (v.Kind == ast.StringValue || v.Kind == ast.BlockValue)
}

// jsonTag renders the json struct tag Config.JSONTags asks for, as ` json:"name"` (with
// ",omitempty" for a nullable field) and empty when the option is off. It is empty too
// for a field whose Config.TagDirective writes a json tag of its own: reflect answers a
// repeated key with the first, so the generated tag would silently win over the one the
// author spelled out.
func (b *builder) jsonTag(fd *ast.FieldDefinition) string {
	if !b.cfg.JSONTags || fd == nil {
		return ""
	}
	if td := b.cfg.TagDirective; !td.IsZero() {
		for _, d := range fd.Directives.ForNames(td.Name) {
			if k := d.Arguments.ForName(td.KeyArg); k != nil && k.Value != nil && k.Value.Raw == "json" {
				return ""
			}
		}
	}
	if fd.Type != nil && fd.Type.NonNull {
		return " json:\"" + fd.Name + "\""
	}
	return " json:\"" + fd.Name + ",omitempty\""
}

// tagsFor renders the extra struct tags a field's Config.TagDirective directives ask for,
// each as ` key:"value"`, in directive order. It is empty when no directive is configured
// or none is on the field.
func (b *builder) tagsFor(fd *ast.FieldDefinition) (string, error) {
	td := b.cfg.TagDirective
	if td.IsZero() || fd == nil {
		return "", nil
	}
	var out strings.Builder
	seen := map[string]bool{}
	for _, d := range fd.Directives.ForNames(td.Name) {
		k, v := d.Arguments.ForName(td.KeyArg), d.Arguments.ForName(td.ValueArg)
		if k == nil || v == nil {
			return "", fmt.Errorf("@%s needs both %q and %q", td.Name, td.KeyArg, td.ValueArg)
		}
		// Anything but a string was written into the tag as its source text,
		// so `value: null` became the tag "null".
		if !isStringValue(k.Value) || !isStringValue(v.Value) {
			return "", fmt.Errorf("@%s needs string values for %q and %q", td.Name, td.KeyArg, td.ValueArg)
		}
		key, val := k.Value.Raw, v.Value.Raw
		switch {
		// reflect.StructTag ends a key at a space, a quote, a colon or a
		// control character, and gives up on the rest of the tag there: a
		// control character in one key silently hid every tag after it.
		case key == "" || strings.ContainsAny(key, " \"`:") || strings.ContainsFunc(key, unicode.IsControl):
			return "", fmt.Errorf("@%s key %q cannot be a struct-tag key", td.Name, key)
		case key == "graphql":
			return "", fmt.Errorf("@%s key %q is reserved for the field's GraphQL name", td.Name, key)
		case strings.ContainsAny(val, "\"`\\") || strings.ContainsFunc(val, unicode.IsControl):
			return "", fmt.Errorf("@%s value for %q contains a quote, backtick, backslash or control character, which a struct tag cannot carry", td.Name, key)
		case seen[key]:
			return "", fmt.Errorf("@%s repeats key %q", td.Name, key)
		}
		seen[key] = true
		out.WriteString(" " + key + ":\"" + val + "\"")
	}
	return out.String(), nil
}
