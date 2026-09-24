# GraphQL over HTTP conformance

Results of the official [`graphql-http`](https://github.com/graphql/graphql-http)
server audit, the GraphQL Foundation's conformance suite for the GraphQL over
HTTP specification. This is the canonical record; `spec-conformance.md`
summarises it and [`testdata/httpaudit/`](../testdata/httpaudit/) holds the
harness.

Re-run 2026-09-24 against `graphql-http` 1.23.0:

| | total | ok | warn | **error** |
|---|---:|---:|---:|---:|
| **graphql-go**, default | 61 | **58** | 0 | 3 (all `MAY`) |
| **graphql-go**, `WithCSRFPrevention(false)` | 61 | **61** | 0 | **0** |

**Every MUST and every SHOULD passes in both configurations.** The three the
default does not take are the `MAY` rows that send a GET carrying no preflight
header, which is the request shape the CSRF check exists to refuse;
`graphql-http` has no CSRF prevention, so it has nothing to trade away there.
Turning the check off is a deployment decision, not a conformance fix.

The 2026-09-14 run scored 56 ok with 2 warnings and classed those three as
notices. Both warnings are now fixed; see below.

| | total | ok | warn | notice | **error** |
|---|---:|---:|---:|---:|---:|
| graphql-go, 2026-09-14 | 61 | 56 | 2 | 3 | **0** |
| gqlgen (`handler.NewDefaultServer`), 2026-09-14 | 61 | 55 | 5 | 1 | **0** |

**All 13 MUST requirements pass on both.** Neither engine has a conformance
error; the difference is in the SHOULD-level checks.

## The two warnings against graphql-go, and why they were reversed

    SHOULD accept */* and use application/json for the content-type
    SHOULD assume application/json content-type when accept is missing

`gqlhttp` used to treat `*/*` and a missing `Accept` as preferring
`application/graphql-response+json`, on the grounds that it is the media type
the current specification defines. That was a deliberate choice, recorded in
the design document's Phase 1 Deviations, and this document argued for it.

**It was overturned on 2026-09-24.** Running the three reference
implementations rather than reasoning about the specification showed that
`graphql-http` -- the specification's own reference implementation --
`graphql-yoga` and Apollo Server all answer `application/json` to a wildcard
and to a missing header. And the cost here was larger than the media type
alone: `gqlhttp` keys the *status* of a request error on the negotiated type,
so a client that never opted into the newer type was also getting a 400 where
those three give 200. The automatic-persisted-query bug fixed the same week
reached clients through exactly that branch.

`internal/httpreq.Negotiate` now answers `application/json` to both, the audit
takes 61 of 61, and `TestNegotiate` holds the whole table. The original
decision was not unreasonable -- it was made from the specification text
without measuring what the ecosystem does, which is the difference worth
recording.

## Where gqlgen differs

Its five warnings are about status codes and response shape on failure:
returning a non-200 status on parse, validation and variable-coercion failures
when the client accepts `application/json`, and including a `data` entry on
parse and validation failures when the client accepts
`application/graphql-response+json`.

## The three MAY rows

`application/x-www-form-urlencoded` GET bodies, and URL-encoded JSON string
`variables` parameters in GETs under either media type. All three are one
cause: CSRF prevention rejects a GET with no preflight header, which is what
those rows send. With the check off all three pass. GET with URL-encoded JSON
variables works either way when a preflight header is present:

    GET /?query=query($v:String){echo(v:$v)}&variables={"v":"hi"}
    graphql-require-preflight: 1
    -> {"data":{"echo":"hi"}}

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
