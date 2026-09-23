# Why the root package is one package

**Why the root package is 32 files and ~9.5k lines, and why that is not a factoring
problem.** Go forbids import cycles and scopes encapsulation to the package, so any concern
that reads the plan compiler's or the executor's unexported types has to live beside them.
Checked rather than assumed: `authz_shape.go` touches seven of them (`selectionSet`,
`planField`, `objectType`, `fieldDef`, `abstractType`, `schemaBuilder`, `plan`), `authz.go`
and `authz_exec.go` four each, `limits.go` three, `introspection.go` one. Moving authorization
out — 1,664 lines, 17% of root — would mean exporting the plan compiler's internals or
re-creating `AuthSite`'s unexported fields (`leaf`, `argType`, `argValue`, `valueNonNull`)
through constructors, which is strictly worse than the file prefix it has now. `authz_input.go`
is the one exception, with no engine coupling at all; it is the natural seam if a split is
ever forced, and nothing else is.

The three implementations worth comparing against agree with this. The counts below are from
grpc-go 1.85.0-dev (e4711283) in `ref/`, which is gitignored and not part of any module, so a
fresh clone cannot check them and they will drift; the shape of the answer is the part that
matters, not the digits. The async-graphql and graphql-js readings are from docs.rs and
graphql-js.org/api-v17 respectively, not from source. **grpc-go** is the only
true peer, and its root is *larger*: 22 non-test files and 10,390 lines against this
repository's 32 and 9,544. What grpc-go keeps out of root is vocabulary (`codes` 361 lines,
`status` 162, `metadata` 426, `keepalive` 99) and implementations — `grpc-go/authz` is a
`StaticInterceptor`/`FileWatcherInterceptor` built on the interceptor mechanism that stays in
root, which is the analogue of this repository's unbuilt `ext/authz`, not of its spine.
**async-graphql** keeps `Guard`, `Schema` and the executor in one crate and splits out only
parser, value and derive — the analogue of depending on `gqlparser`. **graphql-js** is the
one that looks different, with `language`, `type`, `execution`, `validation` and `error` as
submodules, but its root re-exports all of them and JavaScript permits the cyclic module
references that make that split possible. Go does not, so copying its shape would buy nothing
and cost the encapsulation that keeps `planField` at 176 bytes.
