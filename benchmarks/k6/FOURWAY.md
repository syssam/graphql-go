# Four engines over HTTP

`transports.js` compares this repository's transports with each other and found
they do not separate over a socket. This is the other question: **do the
engines separate?** They do, by more than the socket costs, which is the only
reason k6 can answer it.

Run 2026-09-24, Docker on one 20-core Windows host.

| engine | rps (median of 3) | round spread | median | p90 | p99 | vs ours |
|---|---:|---:|---:|---:|---:|---:|
| **graphql-go** | **40 791** | 2.9% | 0.27 ms | 0.64 ms | 1.64 ms | 1.0x |
| gqlgen 0.17.95 | 3 517 | 1.2% | 4.16 ms | 7.13 ms | 10.69 ms | 11.6x |
| graphql-http 1.23.0 (graphql-js 16) | 3 038 | 2.8% | 4.78 ms | 6.42 ms | 10.75 ms | 13.4x |
| Apollo Server 5.5.1 | 2 133 | 3.5% | 6.95 ms | 8.51 ms | 15.16 ms | 19.1x |

Zero failed checks anywhere: every response was 200 and carried no `errors`.

## Why this comparison is answerable when the transport one was not

The transport suite could not separate `net/http` from fasthttp because the
difference (~4.6 µs) was smaller than a loopback round trip (66-118 µs). Here
the difference is 4-7 ms. An effect an order of magnitude *above* the noise is
what k6 can measure; one below it is not.

## What was done to make it fair

**One dataset, proven identical.** `cmd/compareserver` serves both Go engines
from a single `data.Dataset()` call, and the Node servers rebuild it with the
same wrap (`(i+j+1) % 100`). Before measuring, all four were asked the same
nested query and the response bodies hashed: graphql-go, gqlgen and
graphql-http were **byte-identical**, and Apollo differed by one trailing
newline. A four-way benchmark against `cmd/transportserver` would have compared
fixtures -- it builds its own dataset with different ids (`u0` against `1`).

**Equal CPU, in separate containers.** Every server got `--cpus=2`; k6 got its
own container and its own cores. Node runs JavaScript on one thread whatever it
is given. That is a property of the runtime, reported rather than corrected
for: it is also what you get when you deploy one Node process per pod.

**Interleaved rounds**, all four then all four again, for the reason in
`verification.md`: contention arriving mid-run otherwise lands on one engine.
Round-to-round spread came out at 1.2-3.5%, so it worked.

**Warmed first.** Node has to JIT and graphql-go has to compile a plan; neither
belongs in a steady-state number.

**A closed model**, constant VUs rather than a constant arrival rate. An open
model on a shared machine reports the generator's own dropped iterations as if
they were the server's.

## The absolute numbers are a floor, not a capacity

Giving k6 more cores and more VUs made measured throughput **fall**:

| k6 cpus | VUs | graphql-go rps |
|---:|---:|---:|
| 4 | 16 | 38 936 |
| 8 | 32 | 35 763 |
| 12 | 64 | 28 025 |

At 40 000 rps the generator and the server are competing for the same host, so
the server's share shrinks as the generator's grows. This is the same effect
`README.md` records from the transport suite. **Ratios from interleaved rounds
travel; absolute numbers do not.**

## Cross-checked in process

An 11.6x gap over HTTP is large enough to be worth disbelieving, so it was
checked against a measurement that shares none of the HTTP machinery -- the
existing in-process benchmark, same container, same `--cpus=2`, same schema:

```
BenchmarkGraphQLGoShallow-2    15 500 ns/op      6 116 B/op      18 allocs/op
BenchmarkGQLGenShallow-2      195 000 ns/op    248 483 B/op   4 976 allocs/op
```

**12.6x in process against 11.6x over HTTP.** The two agree, and HTTP
compressing the ratio slightly is the expected direction, since the socket
costs both engines the same. The mechanism is in the allocation column: 18
against 4 976 for the same response.

`performance.md` quotes 5.6x under saturation for a *two-field* query; this is
a 100-element list with three fields each, so gqlgen pays per resolved field
where the compiled plan does not. The two figures are different query shapes,
not a contradiction.

## Reproducing

From this directory (`benchmarks/k6`). The paths are relative to it: `..` is the
`benchmarks` module and `../..` is the repository. Getting that wrong is how the
first version of this section shipped a build command that failed with
`directory not found`; these were run before being written down.

```sh
docker network create gqlbench

# One binary, both Go engines, one dataset.
docker run --rm -v "$PWD/../..:/src" -w /src/benchmarks golang:1.27 \
  go build -o /src/benchmarks/srv-cmp ./cmd/compareserver

docker run -d --name srv-ours   --network gqlbench --cpus=2 -v "$PWD/..:/b" \
  golang:1.27 /b/srv-cmp -engine ours   -addr :18090
docker run -d --name srv-gqlgen --network gqlbench --cpus=2 -v "$PWD/..:/b" \
  golang:1.27 /b/srv-cmp -engine gqlgen -addr :18095

# Node. Run npm install in k6/node once first.
docker run -d --name srv-apollo --network gqlbench --cpus=2 -v "$PWD/node:/app" \
  -w /app -e PORT=18096 node:22 node /app/apollo.mjs
docker run -d --name srv-ghttp  --network gqlbench --cpus=2 -v "$PWD/node:/app" \
  -w /app -e PORT=18097 node:22 node /app/ghttp.mjs
```

Check the fixture before believing any number. Three of the four bodies must
hash the same and Apollo's must differ only by a trailing newline:

```sh
for t in srv-ours:18090 srv-gqlgen:18095 srv-apollo:18096 srv-ghttp:18097; do
  docker run --rm --network gqlbench curlimages/curl -s -X POST \
    -H 'content-type: application/json' -H 'accept: application/json' \
    -d '{"query":"{ users { id name email friends { id name } } }"}' \
    "http://$t/graphql" | md5sum
done
```

Then warm each server and alternate the four; do not run one to completion
before starting the next.

```sh
docker run --rm --network gqlbench --cpus=4 -v "$PWD:/k6" \
  -e URL=http://srv-ours:18090/graphql -e SIZE=shallow -e VUS=16 -e DURATION=15s \
  grafana/k6 run /k6/fourway.js
```

`SIZE` is `tiny`, `shallow` or `nested`. `fourway.js` prints one line of JSON,
so a shell loop can collect rounds.
