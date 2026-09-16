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
// level also use a reflective traverser recorded at start-up. A composite
// list field may return iter.Seq[E] or iter.Seq[*E] wherever it may return
// []E or []*E.
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
//
// Authorization is a compiled pass over the plan, not a wrapper around
// resolvers, and keeps three concerns separate: what an operation touches,
// what a principal may do with it, and how that decision is enforced. A
// schema author marks a field with @requiresScopes(scopes: [[String!]!]!),
// an OR of ANDs read into a Requirement; NewSchema rejects a malformed
// scopes value. At plan compile, buildAuthShape walks the plan once and
// records every such field as a Site in an AuthShape, cached with the plan
// itself -- the shape does not depend on who is asking, so the plan cache is
// not multiplied by policy, and a field that declares nothing carries
// authIdx -1 and costs one integer compare on the request path. An
// Authorizer turns a shape into a Decision once per operation -- and, for a
// subscription, once when the stream opens (gating the source itself) plus
// once more per event (gating that event's Response), so a scope revoked
// mid-stream takes effect on the next event without tearing the subscription
// down; a refused event is an error response, not a closed stream.
// Decision.Set assigns each Site an Outcome: Allow (the zero value), Deny
// (an AIP-211-worded error that reveals neither the value nor whether the
// resource exists), Null, Zero (a non-null field's zero value, for
// withholding without null-bubbling its parent), or Redact (resolves the
// field and rewrites the value); Drop is defined but always rejected as not
// yet implemented. Deny, Null and Zero write their response without calling
// the resolver, so a FieldInterceptor never observes those fields; Redact
// does resolve, so a FieldInterceptor sees the pre-redaction value.
//
// WithSubscriptionInterceptor wraps the opening of a subscription's stream,
// the way OperationInterceptor wraps a query or mutation -- gRPC's
// Unary/Stream split. It exists because an OperationInterceptor only sees a
// subscription's individual events, by which point the source is already
// open and refusing the subscription outright is no longer possible.
// Relatedly, NewSchema rejects a bound directive (one that wraps a field
// executor) on a subscription root field: the per-event writer substitutes
// that field's executor outright to yield the event, so a directive bound
// there would compose cleanly and then never run.
//
// RequireAuthCoverage is an opt-in, build-time default deny: NewSchema fails
// for any bound field that declares neither @requiresScopes nor @public.
// It is a Go-vs-SDL shape check, not proof that every declared requirement
// is enforced. SiteKind names four kinds of position -- SiteOutput,
// SiteObject, SiteFilterArg, SiteInputWrite -- but only SiteOutput, a single
// field's own definition, is ever built into a Site today; a @requiresScopes
// on an object type or on an interface field definition creates no Site,
// authorizes nothing, and does not satisfy coverage for a concrete field
// that omits its own declaration, even though the schema reads as guarded.
// Filter-argument and input-write sites are declared for the same reason and
// are likewise not yet populated.
package graphql
