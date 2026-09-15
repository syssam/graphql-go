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
