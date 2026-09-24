# Upstream patches

Patches against dependencies, kept here until they land upstream. The root
package may depend only on `gqlparser/v2` and the standard library, so a
specification gap inside the parser cannot be fixed in this repository — it is
fixed there and submitted.

Each patch applies to a clean checkout of the dependency's default branch and
passes that project's own test suite. Nothing here is applied automatically;
`go.mod` continues to point at the released version.

**A library cannot ship a patched dependency.** A `replace` directive applies
only to the main module and is ignored in everything that depends on it, so
these cannot be wired in here for consumers even temporarily. Upstream is the
only path that reaches anyone; the `replace` described at the end is for trying
them in an application you control.

## 0001-gqlparser-unicode-escapes.patch

Fixes two September 2025 Edition gaps in `lexer.readString` — see gaps 1 and 2
in [`../spec-conformance.md`](../spec-conformance.md).

```sh
git clone https://github.com/vektah/gqlparser.git
cd gqlparser
git apply ../graphql-go/docs/upstream/0001-gqlparser-unicode-escapes.patch
go test ./...
```

## 0002-gqlparser-executable-descriptions.patch

Fixes gap 3: `Description?` on `OperationDefinition` and `FragmentDefinition`.
Independent of 0001 -- they touch different files and apply in either order.

## 0003-gqlparser-directives-on-directive-definitions.patch

Fixes gap 4: the draft's `DIRECTIVE_DEFINITION` location, directives applied to
a directive definition, and `DirectiveExtension`. Also updates gqlparser's
prelude to match. Independent of 0001 and 0002; all three apply in any order.

## 0004-gqlparser-formatter-descriptions.patch

`formatter.FormatSchema` output does not always reload as the same schema. It
drops the schema description. It writes a description holding `"""`
unescaped, so the output does not parse. It writes every description as a
block string, which strips leading and trailing blank lines and common
indentation. This is not a specification gap, and this repository no longer
needs the patch: `PrintSDL` prints through `internal/sdlprint`, which
carries the same fixes. It is kept for everyone else on the formatter.
gqlgen is one of them: its `@inlineArguments` path re-serializes the schema
through `FormatSchema` and embeds the result. Run against gqlgen v0.17.95, a
`"""` description there panics the generated package's `init`, and the schema
description and description whitespace are lost from introspection. It is
independent of 0001–0003.

Re-checked on 2026-09-24 against `dc52fbe`: each of the four applies cleanly on
its own, and with all four applied `go test -race ./...` passes in every
package.

## Verification, 2026-09-22

Re-checked against `vektah/gqlparser` at `991cd11` (2026-09-21, the commit
after `v2.5.37`), because a patch that no longer applies is worse than none:
it reads as ready and is not.

| | Result |
|---|---|
| `git apply --check`, all three | clean |
| `go test ./...` with all three applied | **7 packages, all pass** |

The behaviour each is for, before and after, read out of the parser rather than
out of the patch:

| Query fragment | v2.5.37 | patched | September 2025 Edition |
|---|---|---|---|
| `"\uD83D\uDE00"` | two U+FFFD | U+1F600 | U+1F600 |
| `"\uD83D"` alone | one U+FFFD | syntax error | syntax error |
| `"\u{1F600}"` | rejected | U+1F600 | U+1F600 |

The first row is the one to lead a pull request with. It is not a formatting
difference: **any client sending an emoji or other astral character as an
escape has its data silently replaced**, and nothing in the response says so.

The `autocrlf` caveat below was re-confirmed the hard way in the same session.
A default Windows clone failed three `formatter` baselines; the same checkout
cloned with `core.autocrlf=false` passed everything. If those three are the
only failures, the line endings are the cause and not the patch.

## Trying them before they merge

Apply the patches to a checkout and point `go.mod` at it with a `replace`. Do
not commit that `replace`.

On Windows, clone and apply with `core.autocrlf=false`. With the usual
`autocrlf=true`, `git apply` rewrites the formatter's golden files to CRLF
while the test regenerates them as LF, and every baseline comparison fails on
line endings alone.
