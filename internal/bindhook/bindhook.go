// Package bindhook lets this module's own packages ask the schema builder a
// question the public API has no word for.
//
// fed.Resolver[E] names an entity type and the Go type its resolver returns,
// and whether E is the Go type that entity is bound to is known only to the
// builder. The check is a schema option, so that it runs with every other
// build check and reports with them, but an option is a closed type only the
// root package can make. The root package puts the constructor here at init;
// fed, which cannot import an unexported one, calls it.
package bindhook

import "reflect"

// ObjectBoundTo returns a graphql.SchemaOption, as any, that fails the build
// unless the object type name is bound to Go type t. what describes who is
// asking, for the error. It is set by the root package's init and is never
// nil once that package is imported.
var ObjectBoundTo func(what, name string, t reflect.Type) any

// CoverageExempt returns a graphql.SchemaOption, as any, that leaves the
// fields named, each as "Type.field", out of what RequireAuthCoverage
// demands. It is for a field a package of this module adds to a schema on
// the author's behalf: the author cannot write a requirement or @public on
// text that is not theirs, and a coverage check that refuses it is one they
// cannot satisfy. It exempts from the check and from nothing else -- a
// requirement on the field's type guards it as it guards any other.
var CoverageExempt func(coords ...string) any
