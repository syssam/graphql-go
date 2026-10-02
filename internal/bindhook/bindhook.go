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
