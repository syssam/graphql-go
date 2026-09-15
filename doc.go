// Package graphql is a schema-first GraphQL runtime for Go with a code-first,
// type-safe binding API.
//
// A Schema is built from SDL plus bindings that connect GraphQL types to Go
// types: Object, Field, Resolve, Input, Args, Enum, Scalar, Interface, Union
// and Directive. Generated code and hand-written code use the same
// constructors, so code generation is a convenience rather than a
// requirement. Bindings are validated against the SDL once, at NewSchema.
//
// After start-up, output fields, explicit InputField setters and scalar
// writers are typed function values. Args[T]() / Input[T](name) may use
// reflection once per input value to fill struct fields; that is decode
// only and is not on the JSON write path. Composite lists deeper than one
// level also use a reflective traverser recorded at start-up.
//
// An Executor compiles each operation into an immutable, cached plan and
// writes the response directly into a pooled JSON buffer. Fields bound with
// Field are treated as pure data access and run inline; fields bound with
// Resolve may perform I/O and are scheduled concurrently under a bounded
// semaphore. The loader package batches I/O across those resolvers in one
// request through WaveCoordinator;
// WithMaxComplexity, WithMaxDepth and WithQueryCost reject expensive
// operations before they run.
//
// Subscriptions are bound with Subscribe and SubscribeArgs, which return the
// source channel for a subscription root field. Executor.Subscribe runs one
// such operation and yields a Response per event, executing the field's
// sub-selection against the event exactly as a query would be executed
// against a resolver's result.
package graphql
