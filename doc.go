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
// scopes value. That spelling is the default rather than the only one:
// RequirementDirective(name, arg, shape) declares another SDL directive read
// into a Requirement, so a schema that already spells its requirements
// @auth(requires: [String!]) is enforced without renaming every site. The
// shape is declared rather than inferred, because @auth(requires: ["a","b"])
// is the same text whether the author meant AND or OR and guessing wrongly
// widens access. MarkerDirective(name, scope) covers a directive with no
// argument, whose presence alone is the requirement, which is the shape of
// Apollo's @authenticated. @requiresScopes and @authorizeInput only describe positions
// in the schema; nothing is enforced against them unless an Authorizer is
// configured with WithAuthorizer. At plan compile, buildAuthShape walks the
// plan once and records every such field, and every field that selects an
// @authorizeInput argument, as a Site in an AuthShape, cached with the plan
// itself -- the shape does not depend on who is asking, so the plan cache is
// not multiplied by policy, and a field that declares nothing and selects no
// @authorizeInput argument carries authIdx -1 and costs one integer compare
// on the request path. An Authorizer turns a shape into a Decision once per
// query or mutation -- and, for a subscription, once when the stream opens
// plus once more per event.
// At open, exactly three things refuse the subscription before its source is
// opened: an Authorize error, a Deny recorded for the subscription root
// field, or a Deny recorded for one of that field's argument sites; any
// other outcome on the root, and every outcome below it, is left to the
// per-event pass. That pass gates each event's Response, so a scope
// revoked mid-stream takes effect on the next event without tearing the
// subscription down; a refused event is an error response, not a closed
// stream.
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
// for any bound field that declares neither @requiresScopes nor @public. A
// field's effective requirement is the AND of its own @requiresScopes, its
// object type's, and each interface it implements -- both that interface's
// type-level requirement and its same-named field's. resolveAuthRequirements
// computes it once, at NewSchema, and stores the result on the field's
// fieldDef and the object's objectType; the shape builder and
// RequireAuthCoverage both read those stored values, so what a request
// enforces and what coverage accepts cannot diverge. __typename is guarded
// the same way, through a SiteObject built from its object's effective
// type-level requirement; Decision.Set admits only Allow and Deny there,
// since __typename is String! and has no value to null, zero or redact. That
// guard is for consistency, not confidentiality: authorization at the field
// or instance level does not hide how many objects of a guarded type exist or
// that they exist, and an object whose selection folds to empty is written
// without consulting any site.
// __schema and __type are fields of the query root, so an @requiresScopes on
// the Query type guards introspection as well; that fails closed on purpose.
// @requiresScopes anywhere the engine does not enforce it -- a union, enum,
// enum value, scalar, input object, input field, an argument, the schema
// definition itself, or a directive definition's own argument -- is a build
// error, not a silent no-op. RequireAuthCoverage counts a field as covered
// exactly when its effective requirement is non-zero, or when the field or
// its own object type carries @public; @public on an interface does not
// exempt its implementers. An argument marked @authorizeInput(kind: FILTER |
// WRITE) gets a SiteFilterArg or SiteInputWrite site on every field that
// selects it, whether or not the client supplied a value for it.
// Decision.Input reports what the client actually supplied there:
// input-object key paths, enum values and explicit nulls, but never a scalar
// value, since a policy decides on schema identifiers and scalars are user
// data; a default an operation variable itself carries counts, because the
// operation's author chose it, while a default the SDL supplies for the
// argument or an input field does not, because the client never chose it.
// Only Allow and Deny apply to an argument site; Deny refuses the field
// before its resolver runs, whatever outcome is recorded for the field's own
// output site. ScopeAuthorizer leaves every argument site at Allow, since its
// Requirement is zero and mapping a key or enum value to the field it
// restricts is the consumer's naming convention rather than the engine's;
// RequireAuthCoverage does not require an argument to declare
// @authorizeInput, for the same reason.
// Individual values are decided separately. An object type marked
// @authorizeObject has every value of it offered to the ObjectAuthorizer
// registered with WithObjectAuthorizer before it is written; without one the
// marker describes positions and nothing is enforced. These decisions cannot
// live in a Decision, which is built before any resolver runs and so cannot
// name values that do not exist yet. Checks are batched per list -- a list of
// guarded values is drained and decided in one call before any element is
// written, split only by WithObjectAuthBatch (50 by default) -- while a
// guarded object reached through a field that is not a list is one call
// carrying one check. Allow, Deny, Null and Drop apply; Null only where the
// position is nullable, and Drop only to a list element, where it omits the
// value and renumbers the indices that follow, so a later denial is reported
// at the index the client actually sees. A batch that fails -- an error, a
// mismatched length, an outcome the site cannot represent, a panic -- fails
// every check still outstanding rather than allowing the ones a later batch
// would have covered. The marker is rejected at NewSchema anywhere the
// executor would not consult an ObjectAuthorizer, a root operation type
// included. An instance site appears in AuthShape.Sites so an Authorizer can
// see that instance checks will happen, but its outcome comes from the
// ObjectAuthorizer: Decision.Set refuses it, and an operation whose only
// sites are instance sites does not consult the Authorizer at all.
//
// An Authorizer failure that is not a *Error -- typically a policy backend's
// own transport failure -- is never shown to a client: it is presented as a
// generic internal error, with the original logged and kept behind an
// unexported cause for a presenter that wants to test it.
package graphql
