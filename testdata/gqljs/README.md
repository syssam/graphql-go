# The graphql-js differential

`expected.json` is [graphql-js](https://github.com/graphql/graphql-js) 17.0.2's own output for
every case in `cases.json`, recorded by `run.mjs`. `TestGraphQLJSDifferential` (root package)
builds the same schema in this engine, runs the same cases, and requires the two to agree.

It runs in CI like any other Go test — Node is needed only to re-record.

## Re-recording

```sh
cd testdata/gqljs
npm i graphql@17        # or whichever version you are recording
node run.mjs > raw.json
```

`run.mjs` prints `{name, hasData, data, errors, errorCount}` per case; `expected.json` is that
with `errors` split into `errorPaths` and `messages`. Keep `run.mjs` and the Go fixture in
`gqljs_conformance_test.go` in step — they are two spellings of one schema, and the test is
worth nothing if they drift.

## What is compared, and what is not

Compared: whether `data` is present, its exact shape, how far a non-null error bubbled, the
number of errors, and the path on each one.

**Message wording is recorded but not compared.** Eight differ. Four are byte-identical to
graphql-js **16** and differ only because 17 reworded them, which is gqlparser's phrasing
showing through:

| case | 16 and this engine | 17 |
| --- | --- | --- |
| missing required variable | `Variable "$n" of required type "Int!" was not provided.` | `Variable "$n" has invalid value: Expected a value of non-null type "Int!" to be provided.` |
| null for a non-null variable | `Variable "$n" of non-null type "Int!" must not be null.` | `Variable "$n" has invalid value: Expected value of non-null type "Int!" not to be null.` |
| input object missing a field | `Field "Inp.n" of required type "Int!" was not provided.` | `Expected value of type "Inp" to include required field "n", found: {  }.` |
| input object unknown field | `Field "nope" is not defined by type "Inp".` | `Expected value of type "Inp" not to include unknown field "nope", found: { n: 1, nope: 2 }.` |

Three more are gqlparser's: a trailing ` at $n.` on a variable coercion error, a dropped
`Syntax Error: ` prefix on a parse failure, and a shorter "Did you mean" list on an enum.

Exactly one is this engine's: an argument coercion failure is prefixed with
`Invalid argument for field Query.intArg: field "v": `. The error carries a source location as
graphql-js's does, so the prefix is extra context rather than a substitute for it.

**Error order is compared.** It was not at first: graphql-js reported in document order and
this engine appended as concurrent resolvers finished, so the same query produced a different
order run to run — 200 executions of one six-error query gave 150 distinct orderings, while
`data` was stable. That was the one behavioural difference this differential found, and the
engine now sorts (`sortErrorsByDocumentOrder`, `TestErrorsAreInDocumentOrder`).

## A warning about this harness

The first run reported five differences. All five were bugs in the *Go fixture*, not the
engine: `Other` was bound to the same Go type as `Thing` (so the abstract resolver was
genuinely ambiguous), `grid` returned a nil inner slice where graphql-js returned a list
containing null, `listNonNullElems` returned an error where graphql-js returned a list
containing null, and `nullFromNonNull` returned `""` because a Go `string` cannot be null —
it needs `*string`. A differential is only as good as the fixture on both sides, and a
difference is a question, not a finding.
