# Upstream patches

Patches against dependencies, kept here until they land upstream. The root
package may depend only on `gqlparser/v2` and the standard library, so a
specification gap inside the parser cannot be fixed in this repository — it is
fixed there and submitted.

Each patch applies to a clean checkout of the dependency's default branch and
passes that project's own test suite. Nothing here is applied automatically;
`go.mod` continues to point at the released version.

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

## Trying them before they merge

Apply the patches to a checkout and point `go.mod` at it with a `replace`. Do
not commit that `replace`.
