# Transport load tests

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

## Where the difference is real

The Go benchmarks do separate them, because they measure CPU and allocations
rather than wall time through a socket.

In process, tiny payload — this is almost entirely transport:

| | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| net/http | 1 762 | 1 430 | 20 |
| echo v5 | 2 102 | 1 456 | 21 |
| **fiber v3 native** | **1 585** | **892** | **18** |
| fiber v3 via adaptor | 6 211 | 4 172 | 40 |

Over loopback, list payload — connection reuse and header parsing included:

| | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| net/http | 118 954 | 18 982 | 223 |
| echo v5 | 106 193 | 18 931 | 224 |
| **fiber v3 native** | **93 562** | **14 471** | **192** |
| fiber v3 via adaptor | 123 713 | 19 672 | 218 |

Fiber's native path allocates 24% fewer bytes and 14% fewer objects per request
over the wire than net/http. That is the part that survives the trip to another
machine; the nanoseconds are not.

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
that is a 3.9x penalty on time and 4.7x on bytes.

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
