# The GraphQL-over-HTTP audit

`graphql-http` ships the specification's own compliance suite. `run.mjs` points it at a URL
and prints the result by severity. Unlike `testdata/gqljs`, this one cannot be reduced to a
golden file — the suite drives the requests itself — so it is run by hand, and what it checks
that this repository changed is held by Go tests named below.

```sh
cd testdata/httpaudit
npm i graphql-http graphql
go run ./server.go &          # any gqlhttp server on the URL below works
node run.mjs http://127.0.0.1:4151/
```

## Result, 2026-09-24, graphql-http 1.23.0

| configuration | ok | warn | error |
| --- | --- | --- | --- |
| `gqlhttp.New(exec)` (the default) | 58 | 0 | 3 |
| `gqlhttp.New(exec, gqlhttp.WithCSRFPrevention(false))` | **61** | 0 | 0 |

**Every MUST and every SHOULD passes in both.** The three the default configuration does not
take are all `MAY`, and all three are one cause: CSRF prevention rejects a GET that carries no
preflight header, which is what those rows send.

- `MAY accept application/x-www-form-urlencoded formatted GET requests`
- `MAY allow URL-encoded JSON string {variables} parameter in GETs` (both media types)

`graphql-http` has no CSRF prevention at all, so it has nothing to trade away here. Turning the
check off is a deployment decision, not a conformance fix, and GET with URL-encoded JSON
variables works with a preflight header:

```
GET /?query=query($v:String){echo(v:$v)}&variables={"v":"hi"} -- graphql-require-preflight: 1
{"data":{"echo":"hi"}}
```

## What the suite found

Two `SHOULD`s, both since fixed, both about a client that named neither JSON type:

- `SHOULD accept */* and use application/json for the content-type`
- `SHOULD assume application/json content-type when accept is missing`

This engine answered `application/graphql-response+json` to both, and because the media type
also decides whether a request error is 200 or 400, those clients were getting a 400 they never
opted into. `httpreq.Negotiate` now answers `application/json`, which is what graphql-http,
graphql-yoga and Apollo Server all do. `TestNegotiate` holds the whole table, and
`TestRequestErrorStatusDependsOnAccept` the status that follows from it.
