# Specification conformance

Audited against the [September 2025 Edition](https://spec.graphql.org/September2025/)
and the [working draft](https://spec.graphql.org/draft/) as of draft commit
`2026-06-04`. Every claim below was produced by running a query against a real
executor, not by reading code.

Four gaps remain, and every one of them is in the parser — `gqlparser/v2`,
which the root package is restricted to — rather than in the engine. None can
be fixed here without forking it, and `v2.5.58`, the newest release, carries
all four: re-tested against it on 2026-09-24 after upgrading from `v2.5.37`,
twenty-one releases of which fixed none of them. What was checked on the engine side is listed under *Verified
conformant* below; that section is the extent of the claim, not a statement
that nothing else could be wrong.

Three patches in [`docs/upstream/`](upstream/) close the four gaps — the two
lexer gaps share one. Each applies to a clean gqlparser checkout and passes
that project's suite; all three were also verified applied together, in any
order. They are not applied here — `go.mod` still points at the release — so
the gaps stay open until they land upstream.

**Re-test the gaps, not just the patches.** The 2026-09-24 re-test nearly
recorded gap 1 as fixed upstream. The probe's `\uD83D\uDE00` had reached the Go
source as a literal emoji, so it measured the parser passing a character
through rather than decoding an escape, and of course that worked. Re-authored
so the backslashes survived, it produced two U+FFFD -- what the table below
says. A probe for an escaping bug has to be checked for having been escaped
itself, and the same thing then happened twice more: to the script that wrote
this paragraph, and to an earlier one that truncated this file.

Re-verified on 2026-09-22 against gqlparser `991cd11`, the commit after
`v2.5.37`: all three still apply cleanly, the seven-package suite passes with
them, and the fixed behaviour was read out of the parser rather than off the
patch. [`upstream/README.md`](upstream/README.md) has the before-and-after
table. **They cannot be applied here at all** — a `replace` directive governs
only the main module and is ignored in everything that depends on it, so a
library cannot ship a patched dependency even temporarily. Upstream is the
only route that reaches a consumer.

## Known gaps

### 1. Surrogate escapes are silently corrupted (September 2025)

gqlparser's lexer passes the value of a four-digit escape straight to
`bytes.Buffer.WriteRune`, which turns anything in the surrogate range into
U+FFFD. It implements no pairing rule at all, so **both** halves of the
production are wrong:

| Input | Produces | Specification |
| --- | --- | --- |
| a surrogate pair for U+1F600 | two U+FFFD | U+1F600 |
| a lone leading surrogate | one U+FFFD | syntax error |
| a lone trailing surrogate | one U+FFFD | syntax error |

Section 2.1.7 pairs a leading surrogate with a trailing one and asserts that
every other escaped value lies in the Unicode scalar range (`<= 0xD7FF` or
`>= 0xE000`).

This is the most damaging gap on the list, and the only one that corrupts data
rather than rejecting it: nothing anywhere reports a problem. It is not an edge
case. JSON-based clients routinely emit surrogate pairs for emoji and non-BMP
CJK, so every such character sent inside a query string literal reaches the
resolver as replacement characters.

### 2. Variable-width unicode escapes are rejected (September 2025)

The braced form `\u{1F600}` is a syntax error: `Unexpected <Invalid>`.
`EscapedUnicode :: { HexDigit+ }` is grammar, not an extension, so any client
emitting it cannot talk to this server.

**Gaps 1 and 2 have a patch ready to submit:**
[`docs/upstream/0001-gqlparser-unicode-escapes.patch`](upstream/0001-gqlparser-unicode-escapes.patch).
It rewrites the escape branch of `readString` around a `readEscapedUnicode`
helper covering both productions, adds seven cases to `lexer_test.yml`, and
passes the whole gqlparser suite. Applied underneath this repo it makes every
row of the table above correct and turns a lone surrogate into a parse error.

### 3. Descriptions on executable definitions are rejected (September 2025)

```graphql
"Fetch the current user" query Me { me { name } }   # -> Unexpected String
```

`OperationDefinition : Description? OperationType ...`, and the same for
`FragmentDefinition`. This is what lets a server hand an agent a described
operation, so it matters most for MCP-style tooling.

**A patch is ready to submit:**
[`docs/upstream/0002-gqlparser-executable-descriptions.patch`](upstream/0002-gqlparser-executable-descriptions.patch).
It adds `Description` to both AST nodes, parses the optional description ahead
of each executable definition, emits it from the formatter so documents round
trip, and rejects a description on the shorthand `{ ... }` form, which has no
description slot in the grammar. Six cases in `query_test.yml`, plus a source
file in the formatter's golden corpus so that dropping a description from the
output fails a test rather than passing quietly.

### 4. Directives on directive definitions (draft only, not ratified)

Spec PR #1206, merged to the draft on 2026-04-02, adds the
`DIRECTIVE_DEFINITION` location, directives applied to a directive definition,
and `DirectiveExtension` (`extend directive @x @y`). gqlparser parses none of
the three.

The *introspection* half is implemented here (`patchPrelude` in
`introspection.go`): `__Directive.isDeprecated` / `deprecationReason`,
`__Schema.directives(includeDeprecated:)` and
`__DirectiveLocation.DIRECTIVE_DEFINITION` all exist and answer correctly. They
report `false` and `null` because no schema can yet contain a deprecated
directive -- which is exactly what the specification requires of a service with
none.

**A patch is ready to submit:**
[`docs/upstream/0003-gqlparser-directives-on-directive-definitions.patch`](upstream/0003-gqlparser-directives-on-directive-definitions.patch).
It adds the location, parses applied directives on a directive definition and
the `extend directive` form, applies extensions and validates them (self
reference, allowed location, extending something undefined), emits them from
the formatter in grammar order, and brings gqlparser's own prelude up to the
same revision. Ten cases across `schema_test.yml` in the parser and validator,
plus a source file in the formatter's golden corpus.

When that lands, `introDirective.directives` in `introspection.go` should
return `d.def.Directives` instead of `nil`, at which point a deprecated
directive actually reports as one. Until then the AST has nowhere to hold them
and the honest answer is the one the code gives.

## How the patches were checked

Each was mutation tested: the fix was broken on purpose and the suite had to
fail. That is the habit CLAUDE.md prescribes, and here it earned its keep —
three changes were covered by tests that passed either way:

- directive extensions were parsed and then never applied, and every test still
  passed, because the validator case only asserted that the schema loaded;
- the formatter silently dropped applied directives on a directive definition;
- the formatter silently dropped an operation or fragment description.

All three are round-trip losses that no assertion looked at. The golden corpus
files named above close them: with any of those four mutations in place, a
formatter baseline now fails. A test that cannot fail is not evidence, and
these were headed for another project's repository.

## Measured against other implementations

Two differentials, both run on 2026-09-24. Neither is a guess about what other servers do.

**Execution, against graphql-js 17.0.2.** 41 cases covering null bubbling, error paths,
argument and variable coercion, fragments and operation selection: response shape,
nullability, error count and every error path agree on all 41, in order.
`TestGraphQLJSDifferential` keeps it true from a checked-in recording, so it needs no Node.
[`testdata/gqljs/README.md`](../testdata/gqljs/README.md) has the eight message-wording
differences, seven of which are gqlparser still carrying graphql-js 16's phrasing.

**Introspection, against graphql-js 17.0.2.** The full `getIntrospectionQuery()`
-- descriptions, `specifiedByURL`, `isRepeatable`, schema description and input
value deprecation all on -- run against the same SDL on both, over a schema with
an interface implementing an interface, a union, a custom scalar with
`@specifiedBy`, enum and input-field and field deprecations, and default values
on arguments and input fields.

Thirty differences, and reading them is the point:

| cause | count | verdict |
| --- | ---: | --- |
| `includeDeprecated` is `Boolean` here, `Boolean!` in graphql-js 17 | 15 | the deviation recorded below, at five sites |
| `@deprecated(reason:)` is `String` here, `String!` there | 3 | the same family; the prelude spells it nullable |
| `@deprecated` is not allowed on `DIRECTIVE_DEFINITION` here | 2 | gap 4 |
| `__DirectiveLocation` has `VARIABLE_DEFINITION` where graphql-js 17 splits it into `FRAGMENT_` and `OPERATION_` forms | 4 | graphql-js ahead of the ratified specification: fragment arguments is experimental there |
| `Float` and `ID` are reported though the schema uses neither | 2 | graphql-js prunes unreachable built-ins; this engine reports all five |
| `String`'s description is missing a space | 1 | **a defect, fixed** |
| description on `DIRECTIVE_DEFINITION` | 1 | this engine adds the value, without graphql-js's description text |

Everything except the last two confirmed a decision already written down, which
is what a differential is for. The defect is gqlparser's prelude, which reads
``"The `String`scalar type"``: `patchPrelude` repairs it, because the string
reaches every introspection response and every tool that renders schema
documentation. `TestIntrospectionStringDescriptionHasItsSpace` holds it, and the
replacement is matched exactly so a fixed prelude stops needing it rather than
having correct text corrupted.

**HTTP, against the specification's own audit suite.** 61 of 61 with CSRF prevention off,
58 with it on, every MUST and every SHOULD passing in both.
[`graphql-http-audit.md`](graphql-http-audit.md) is the record and
[`testdata/httpaudit/`](../testdata/httpaudit/) the harness; the numbers are not repeated
here.

**HTTP, against Apollo Server 5.5.1, graphql-yoga 5.24.1 and graphql-http 1.23.0** — the last
being this specification's own reference implementation. Request-error status, by the
negotiated media type:

| | `application/json` | `application/graphql-response+json` |
| --- | --- | --- |
| graphql-http 1.23.0 (reference) | 200 | 400 |
| graphql-yoga 5.24.1 | 200 | 400 |
| **this engine** | **200** | **400** |
| Apollo Server 5.5.1 | 400 | 400 |
| gqlgen (read from source) | 422 | 400 |

A client that sends `*/*`, or no `Accept` at all, is answered `application/json` and therefore
falls in the first column. It named neither type, so it never opted into the newer one, and the
audit requires both halves of that. **A missing `query` member is not in this table**: a body
that carried no query never became an operation, so it is 400 under every media type, which is
what graphql-http does and what `TestEquivalence/missing_query` asserts across three `Accept`
headers. Only a GraphQL request error -- a parse or validation failure -- follows the media
type.

**The 200 is not a legacy accommodation to be tidied away later.** It is what the reference
implementation does, and the rule is the one in `internal/httpreq.Negotiate`: the media type
decides the status. Apollo answers 400 under both and gqlgen 422 under one, so there are three
answers in the field and this engine holds the reference one. A review that finds only Apollo
will conclude this engine is wrong; measure `graphql-http` before changing it.

Two differences that measurement did find were real, and both are fixed: an automatic
persisted-query miss answered 400 to every client that accepts
`application/graphql-response+json` (`apq.IsRetryHandshake`; APQ is Apollo's protocol, not
this specification, and Apollo answers 200 under every media type), and errors came back in
resolver-completion order where all three JS implementations report in document order
(`sortErrorsByDocumentOrder`).

Not adopted: `extensions.code` on errors that carry none. graphql-http and gqlgen add none
either; Apollo and Yoga do, and both leak alongside it — Apollo a full stack trace in the
response body, Yoga the original error under `originalError`. `Error.WithCode` is there for
authors who want one.

## Deliberate deviations

**The prelude's `@defer` is removed.** gqlparser declares it, but incremental
delivery is implemented nowhere in this codebase and is not in the specification
at all. Accepting it would hand a client a single complete response where it
asked for a streamed one, so `patchPrelude` drops it and the validator and
introspection agree that it does not exist.

Only gqlparser's declaration goes. A schema that defines a `@defer` of its own
keeps it, and queries may use it: what that directive means in that schema is
the author's business. `TestExecKeepsUserDeclaredDefer` pins the distinction,
which an unconditional delete silently got wrong.

**`includeDeprecated` is nullable.** The specification's Appendix D says
`Boolean! = false`; the prelude says `Boolean = false`, and the argument added
to `__Schema.directives` matches the prelude for consistency with the three
that were already there. Tightening all four to non-null would be more
conformant and would match graphql-js 17, at the cost of rejecting an explicit
`includeDeprecated: null`. Cosmetic either way — it shows up only in strict
schema-diff tooling.

## Verified conformant

Probed directly, not assumed -- and each claim names the test that keeps it
true. Probing is what someone did once, at audit time, with a throwaway
program; a published conformance claim with nothing guarding it regresses
silently, which is worse than making no claim. Two of the entries below were in
that state until a review added the tests now named.

- `CoerceArgumentValues`, including the `hasValue` correction from spec #1056:
  an argument default applies when its variable is omitted, an explicitly null
  variable stays null, and an omitted argument with no default is absent rather
  than null (`Omittable` makes the distinction visible).
- `@oneOf` input objects from literals *and* variables, across all fourteen rows
  of the section 3.10.1 coercion table (`TestExecOneOfInputObjects`).
- `@deprecated` is refused on required arguments and input fields, section
  3.13.2 (`TestNewSchemaRejectsDeprecatedRequiredInputs`).
- Scalar input coercion: 32-bit `Int` bounds, non-integer rejection, `Float`
  accepting `Int`, `ID` accepting `Int` but not `Float`
  (`TestSpecIDAcceptsIntAndStringOnly` -- including that an *integral* float
  such as `5.0` is still refused, in both literal and variable form).
- Response shape: `message` / `locations` / `path`; `data` absent on a request
  error, `data: null` on a root field error
  (`TestSpecDataIsAbsentOnARequestErrorAndNullOnAFieldError`, which checks the
  serialized envelope as well as the Response, since section 7.1 is about what
  reaches the client).
- Single-root-field and no-introspection-root rules for subscriptions, field
  merging, and introspection depth limiting.
