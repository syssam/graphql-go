# benchmarks

Steady-state runtime comparison of this module against
[gqlgen](https://github.com/99designs/gqlgen) on one shared schema.

See [docs/benchmarks.md](../docs/benchmarks.md) for the latest numbers and
methodology.

```sh
go test -count=1 .
go test -run '^$' -bench . -benchmem -count=5
```

`graph/generated.go` is checked in so `go test` does not need the gqlgen CLI.
Regenerate after schema changes:

```sh
go generate
```
