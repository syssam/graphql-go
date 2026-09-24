# benchmarks

Steady-state runtime comparison of this module against
[gqlgen](https://github.com/99designs/gqlgen) on one shared schema, plus a
matrix of what each HTTP transport costs on top of the same executor
(`transport_bench_test.go`).

See [docs/benchmarks.md](../docs/benchmarks.md) for the latest numbers and
methodology.

```sh
go test -count=1 .
go test -run '^$' -bench . -benchmem -count=10 > new.txt
benchstat new.txt    # golang.org/x/perf/cmd/benchstat
```

Timings on a warm machine here have moved by 20-77%, and two identical runs of
the transport matrix disagreed by up to 46%. Read `benchstat`'s `±` columns,
not a single sample, and prefer `allocs/op`, which reproduces to the unit.

`graph/generated.go` is checked in so `go test` does not need the gqlgen CLI.
Regenerate after schema changes:

```sh
go generate
```

## Linux-only measurements

Two figures could not be taken on the Windows development machine and are taken
in Docker instead. Both tools live under `cmd/`.

```sh
# Latency percentiles. Reports the clock tick first and refuses to report a
# percentile the clock cannot carry, which is what happens on Windows.
docker run --rm -v "$PWD/..:/src" -w /src/benchmarks golang:1.27 \
  sh -c 'go run ./cmd/latency -n 200000 -c 1'

# Behaviour under a cgroup memory limit. Pass --memory-swap as well: --memory
# alone grants an equal amount of swap, and the process then survives well past
# the limit a pod would enforce.
docker run --rm --memory=256m --memory-swap=256m -e GOMEMLIMIT=200MiB \
  -v "$PWD/..:/src" -w /src/benchmarks golang:1.27 \
  sh -c 'go run ./cmd/memlimit -users 150000 -c 4'
```

Results are in [`docs/performance.md`](../docs/performance.md#latency-percentiles)
and [`docs/operations.md`](../docs/operations.md#in-a-container). The short
version: on Linux the clock tick is 17 ns against Windows' 211 µs, so the
percentiles are real; and Go reads the cgroup CPU limit but not the memory
limit, so a container without `GOMEMLIMIT` is OOM-killed on a workload it can
otherwise serve.
