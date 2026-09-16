# k6 load tests

An external load generator against both engines' servers, so the HTTP figures
do not all come from Go benchmarks driving `httptest`.

```sh
cd compare && go run gen.go -n 200
go build -o srv-gqlgo.exe ./graphqlgo/cmd/graphqlgo-server
go build -o srv-gqlgen.exe ./gqlgen/cmd/gqlgen-server
./srv-gqlgo.exe  -addr :18080 &
./srv-gqlgen.exe -addr :18081 &

k6 run -e URL=http://localhost:18080/graphql -e ENGINE=graphql-go k6/load.js
```

`MODE=fixed` (the default) offers a rate both engines can attempt and compares
latency at equal load. `MODE=ramp` climbs towards a ceiling. `RATE`,
`DURATION`, `PREVUS` and `MAXVUS` override the defaults.

The six queries are the ones `compare_test.go` uses, so a figure here is
comparable with the rest of the comparison rather than measuring a different
workload.

## Results

Four interleaved rounds, 4 000 req/s offered for 12s each, alternating
engines. Medians:

| | req/s served | avg | median | p90 | p95 |
|---|---:|---:|---:|---:|---:|
| graphql-go | 3 696 | 7.9 ms | 2.2 ms | 10.7 ms | 25.1 ms |
| gqlgen 0.17.95 | 1 970 | 176.4 ms | 62.0 ms | 332.5 ms | 547.8 ms |
| ratio | **1.9x** | **22x** | **28x** | **31x** | **22x** |

At a load graphql-go absorbs while staying under 26 ms at the 95th percentile,
gqlgen serves about half the offered rate and answers the slowest 5% in over
half a second. Zero GraphQL errors from either.

## Why interleaved, and why you should not trust a single run

Rounds were run A, B, A, B rather than all of A then all of B. One round came
out slow for *both* engines — graphql-go 2 354 req/s where it otherwise managed
3 634-3 899, gqlgen 1 102 where it otherwise managed 1 834-3 145. Another
session on the machine was running a 4 GB Go compile at the time. Interleaving
is what keeps that from being attributed to whichever engine happened to be
measured during it.

The same run, taken without interleaving, produced numbers that disagree with
these by more than the effect being measured.

## What these numbers are not

**Not a capacity measurement.** k6 and the server share one 20-core machine, so
every throughput figure is the pair. Raising `maxVUs` from 400 to 2 000 *cut*
measured throughput from 9 017 to 4 925 req/s, because the generator took cores
from the thing it was measuring. A capacity number needs the generator on a
second host; nothing here provides one.

**Not free of contention.** The machine was at 76% load from other work during
these runs. Interleaving makes the comparison survive that; it does not make
the absolute numbers portable.

**Ratios travel, absolute numbers do not.** This is the same rule the rest of
the comparison follows, for the same reason.

## Percentiles, and a correction

`docs/performance.md` says latency percentiles are impossible here because the
monotonic clock has ~522 us of granularity. That is true of an *unsaturated*
request: 70 us of work cannot be timed by a 522 us clock, and every sample
quantises to zero. It is not true under load. Once requests queue, durations
are milliseconds — three orders of magnitude above the tick — and the
percentiles above are real measurements.

The artefact is still visible in the fast engine at low queue depth: a ramp run
reported graphql-go's median as exactly `0s` while gqlgen's was 17.6 ms. A
median of zero is the clock, not the server.

So: percentiles are meaningful under load and meaningless without it, which is
a narrower claim than the one the performance index makes.
