# GraphQL over HTTP conformance

Results of the official [`graphql-http`](https://github.com/graphql/graphql-http)
server audit, the GraphQL Foundation's conformance suite for the GraphQL over
HTTP specification. Run on 2026-09-14 against `graphql-http` 1.23.0, with both
engines serving the same 200-entity schema from `compare/`.

| | total | ok | warn | notice | **error** |
|---|---:|---:|---:|---:|---:|
| **graphql-go** (`transport/gqlhttp`) | 61 | **56** | 2 | 3 | **0** |
| gqlgen (`handler.NewDefaultServer`) | 61 | 55 | 5 | 1 | **0** |

**All 13 MUST requirements pass on both.** Neither engine has a conformance
error; the difference is in the SHOULD-level checks.

## The two warnings against graphql-go

Both are the same deliberate decision:

    SHOULD accept */* and use application/json for the content-type
    SHOULD assume application/json content-type when accept is missing

`gqlhttp` treats `*/*` and a missing `Accept` as preferring
`application/graphql-response+json`, the media type the current specification
defines, rather than the legacy `application/json`. That is recorded as a
Phase 1 deviation in the design document, and the audit classes it as SHOULD
rather than MUST.

The trade-off is real: a client sending `Accept: */*` and assuming
`application/json` comes back will see a media type it did not expect. Change
it if that matters more than signalling the newer type; the behaviour is a
choice, not an oversight.

## Where gqlgen differs

Its five warnings are about status codes and response shape on failure:
returning a non-200 status on parse, validation and variable-coercion failures
when the client accepts `application/json`, and including a `data` entry on
parse and validation failures when the client accepts
`application/graphql-response+json`.

## Notices

Three MAY-level features graphql-go does not implement:
`application/x-www-form-urlencoded` GET bodies, and URL-encoded JSON string
`variables` parameters in GETs under either media type. All optional.

## Reproducing

```sh
cd compare
go run gen.go -n 200
go run ./cmd/servers -graphql-go :18080 -gqlgen :18081
```

Then, in a directory with `graphql-http` installed:

```js
const { auditServer } = require('graphql-http');
const results = await auditServer({ url: 'http://localhost:18080/graphql', fetchFn: fetch });
```

The audit ships no CLI despite what its README implies; `npx graphql-http
audit <url>` fails with "could not determine executable to run", because the
package declares no `bin`. Call `auditServer` from a script instead.

The audit is not wired into CI: it needs Node, and the repository's dependency
discipline is worth more than automating a check whose result changes rarely.
Re-run it when the transport changes.
