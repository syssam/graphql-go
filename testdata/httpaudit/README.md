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

## Results

In [`docs/graphql-http-audit.md`](../../docs/graphql-http-audit.md), which is the
record for both this suite and the 2026-09-14 run against gqlgen. They are not
repeated here: three copies of one table is how two of them go stale.

## What the suite found

Two `SHOULD`s, both since fixed, both about a client that named neither JSON type:

- `SHOULD accept */* and use application/json for the content-type`
- `SHOULD assume application/json content-type when accept is missing`

This engine answered `application/graphql-response+json` to both, and because the media type
also decides whether a request error is 200 or 400, those clients were getting a 400 they never
opted into. `httpreq.Negotiate` now answers `application/json`, which is what graphql-http,
graphql-yoga and Apollo Server all do. `TestNegotiate` holds the whole table, and
`TestRequestErrorStatusDependsOnAccept` the status that follows from it.
