// Package gqlname holds the rule Input[T] uses to derive a GraphQL field name
// from a Go field name when no tag gives one. gqlc has to predict that rule to
// tell whether a Go-bound input field will bind, so the engine and the
// generator share one copy instead of keeping two in step.
package gqlname

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// initialisms are the words a Go name writes in capitals and a GraphQL name
// does not. UUID and GUID come first so that each is taken whole: they end in
// ID, and read as a word followed by one they became uUId and gUId.
var initialisms = strings.NewReplacer("UUID", "Uuid", "GUID", "Guid", "ID", "Id", "URL", "Url")

// FromGo maps AuthorID → authorId, URL → url and UUID → uuid.
func FromGo(name string) string {
	if name == "" {
		return name
	}
	name = initialisms.Replace(name)
	r, w := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(r)) + name[w:]
}
