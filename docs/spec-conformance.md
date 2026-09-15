# Specification conformance

Audited against the [September 2025 Edition](https://spec.graphql.org/September2025/)
and the [working draft](https://spec.graphql.org/draft/) as of draft commit
`2026-06-04`. Every claim below was produced by running a query against a real
executor, not by reading code.

The engine is conformant. The gaps that remain are all in the parser —
`gqlparser/v2`, which the root package is restricted to — and none of them can
be fixed here without forking it. `v2.5.37` is the newest release and carries
all four.

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
description slot in the grammar. Six cases added to `query_test.yml`; the whole
gqlparser suite passes.

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
directive — which is exactly what the specification requires of a service with
none.

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

Probed directly, not assumed:

- `CoerceArgumentValues`, including the `hasValue` correction from spec #1056:
  an argument default applies when its variable is omitted, an explicitly null
  variable stays null, and an omitted argument with no default is absent rather
  than null (`Omittable` makes the distinction visible).
- `@oneOf` input objects from literals *and* variables, across all fourteen rows
  of the section 3.10.1 coercion table (`TestExecOneOfInputObjects`).
- `@deprecated` is refused on required arguments and input fields, section
  3.13.2 (`TestNewSchemaRejectsDeprecatedRequiredInputs`).
- Scalar input coercion: 32-bit `Int` bounds, non-integer rejection, `Float`
  accepting `Int`, `ID` accepting `Int` but not `Float`.
- Response shape: `message` / `locations` / `path`; `data` absent on a request
  error, `data: null` on a root field error.
- Single-root-field and no-introspection-root rules for subscriptions, field
  merging, and introspection depth limiting.
