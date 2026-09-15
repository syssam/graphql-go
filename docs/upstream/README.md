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

To try it under this repository before it is merged, add a `replace` to
`go.mod` pointing at the patched checkout. Do not commit that `replace`.
