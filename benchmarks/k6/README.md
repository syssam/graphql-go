# Transport load tests

> Comparing the **engines** rather than the transports is a different question
> with a different answer, because the effect there is 4-7 ms rather than
> 4.6 µs. See [`FOURWAY.md`](FOURWAY.md): graphql-go, gqlgen, Apollo Server and
> graphql-http over HTTP, cross-checked against an in-process measurement.


k6 against each HTTP transport, so `transport_bench_test.go` is not the only
evidence for what a transport costs.

```sh
go build -o tsrv.exe ./cmd/transportserver
./tsrv.exe -transport fiber -addr :18092 &
k6 run -e URL=http://localhost:18092/graphql -e ENGINE=fiber -e SIZE=list k6/transports.js
```

`-transport` takes `nethttp`, `echo`, `fiber` or `fiber-adaptor`. `SIZE` is
`tiny` (one field, almost entirely transport) or `list` (every user with
friends, almost entirely engine). Run the transports interleaved.

## The finding: over a socket, the transports do not separate

Three interleaved rounds, all four transports, on one 20-core machine.

| offered | payload | what happened |
|---|---|---|
| 3 000/s | tiny | All four serve 3 000/s. Every latency below the clock tick. |
| 6 000/s | list | All four serve 6 000/s. fiber 0.14-0.19 ms avg, net/http and echo 0.32-0.57 ms. |
| 40 000/s | tiny | All four serve 38 600-39 300/s. No transport separates from the others. |

At 40 000 req/s the spread between the fastest and slowest transport is under
2%, and which one is fastest changes between rounds. Dropped iterations vary
from 7 777 to 16 405 on the same transport in different rounds, which is the
load generator and the machine, not the server.

**So: on this hardware, over a real socket, transport choice is not
measurable.** That is the honest result and it is worth more than a ranking.

## Where the difference is real: allocations, not time

These are ten samples through `benchstat`, with the spread shown. An earlier
revision of this file quoted single samples and claimed Fiber was 1.11x faster
in process and 1.27x faster over loopback. **Both were noise**, and the numbers
below retract them.

Time, in process and over loopback. Read the spreads before the values:

| | in-proc tiny | in-proc list | loopback tiny | loopback list |
|---|---:|---:|---:|---:|
| net/http | 1.911us ± 18% | 18.44us ± 51% | 70.67us ± 10% | 107.0us ± 17% |
| echo v5 | 2.465us ± 14% | 18.30us ± 82% | 70.75us ± 11% | 118.0us ± 8% |
| fiber v3 native | 1.907us ± 13% | 15.38us ± 3% | 66.29us ± 8% | 106.0us ± 9% |
| fiber v3 adaptor | 7.842us ± 13% | 23.40us ± 6% | 75.30us ± 13% | 109.7us ± 6% |

**net/http and Fiber's native path are indistinguishable on time**: 1.911us
against 1.907us in process, 70.67us against 66.29us over loopback, with spreads
of 8-18% around both. The in-process list row reaches ±51% and ±82%, which is
not a measurement at all. The one unambiguous time result is the adaptor's
in-process penalty, 7.842us against 1.907us, and its spread is tight enough to
believe.

Allocations, the same ten samples, spreads of 0-4%:

| | in-proc tiny | in-proc list | loopback tiny | loopback list |
|---|---:|---:|---:|---:|
| net/http | 1 396 B / 20 | 6 606 B / 127 | 10.4 KiB / 110 | 17.1 KiB / 222 |
| echo v5 | 1 422 B / 21 | 6 636 B / 128 | 10.5 KiB / 111 | 18.4 KiB / 224 |
| **fiber v3 native** | **892 B / 18** | **6 068 B / 125** | **6.9 KiB / 84** | **13.3 KiB / 192** |
| fiber v3 adaptor | 4 090 B / 40 | 9 585 B / 147 | 10.7 KiB / 110 | 17.2 KiB / 218 |

Fiber's native path allocates 22-36% fewer bytes than net/http at every size,
and over loopback with a tiny payload it makes 84 allocations against 110. That
is the result that holds: it is deterministic where the timings are not, and it
is a mechanism rather than a number -- fasthttp writes into its own buffer
where net/http builds a request object.

So: **Fiber is the best transport here on allocations, and tied with net/http
on speed.** Echo is net/http with a router in front, which is what its numbers
say. Anyone choosing on latency alone should choose on something else.

## The adaptor row, which exists to be falsified

`gqlfiber` is a fasthttp-native implementation rather than `gqlhttp` wrapped in
Fiber's adaptor. The adaptor row is in the suite to test whether that
complexity earns anything, with the mechanism stated in advance: the adaptor
rebuilds a synthetic `*http.Request` per call while the native path writes into
the fasthttp buffer.

It does earn it, and the mechanism is measurable:

| | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| ConvertRequest | 660 | 674 | 8 |
| GoroutineHandoff | 427 | 16 | 1 |
| HandlerOnGoroutine | 2 799 | 1 463 | 21 |

674 bytes and 8 allocations per request, before the handler runs. In process
that is a 4.1x penalty on time (7.842us against 1.907us, ten samples) and 4.6x
on bytes. This is the one place the suite's timings separate anything.

**And k6 cannot see any of it.** At 6 000 and 40 000 req/s the adaptor performs
the same as the native path, within noise. A 4.6us handler difference is
invisible against a loopback round trip. Anyone benchmarking this decision over
HTTP alone would conclude the native implementation was pointless, and would be
measuring the socket.

## What these numbers are not

k6 and the servers share one machine, so no figure here is a capacity
measurement. Raising `maxVUs` from 400 to 2 000 in the sibling engine suite
*cut* measured throughput, because the generator took cores from the thing it
was measuring. Ratios from interleaved rounds travel; absolute numbers do not.
