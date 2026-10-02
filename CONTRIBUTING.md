# Contributing

## The gate

```sh
sh scripts/gate.sh          # vet and test every module
sh scripts/gate.sh -short   # skip the slow subprocess and load tests
```

Run it before opening a pull request, and before pushing run what CI runs:

```sh
scripts/ci-local.sh                 # every CI job, on Linux, in Docker
scripts/ci-local.sh test postgres   # only those
scripts/ci-local.sh flake           # not in CI: the race suite ten times, shuffled
```

It needs Docker and nothing else. The gate on your own machine is one leg of
CI's matrix; this is the Linux leg, which is where the tests that need a
case-sensitive filesystem run, and the PostgreSQL job, which otherwise runs
nowhere but CI. It reads its versions from `go.mod` and the workflow, so the
two cannot disagree; a step added to `.github/workflows/ci.yml` belongs in the
script as well.

`go test ./...` reaches one module and
this repository has five — `benchmarks/`, `compare/`, `lint/` and
`examples/backoffice/` are outside the root module, so a root-only run reports success while testing none of them.

`-race` is not optional. The DataLoader N+1 race was caught 4 times in 40 runs
with it and 0 times in 40 without: the detector perturbs scheduling enough to
hit the interleaving. `testing/synctest` makes it worse, not better — its
deterministic scheduling hid the same bug in 20 of 20 runs.

## Tests that can fail

A test that passes against deliberately broken code is agreeing with it rather
than checking it. Break the thing your test is about, confirm the test fails,
then fix it. This has caught, among others, a subscription leak test that
passed against a broken release path because a second redundant path still
freed everything, and a benchmark reporting 45ns per subscriber while timing a
publish that dropped the events it claimed to measure.

For generated code, compile it. Two separate defects shipped through review as
text that read correctly and did not build.

## Performance claims

Compare with `benchstat`, at `-count=10`, with both sides measured in the same
sitting:

```sh
go test -count=10 -run '^$' -bench . -benchmem > old.txt
go test -count=10 -run '^$' -bench . -benchmem > new.txt
benchstat old.txt new.txt
```

Single samples here have been wrong by 20-77% on a warm machine, and a
non-interleaved comparison once reported a 13.8% regression that vanished at
n=18. Allocation counts are deterministic and are the figures to trust when
timings are noisy.

Adding a field to `execState` or `OperationContext` is a performance change:
both are allocated per request and both sit on a size-class boundary. Check
with `unsafe.Sizeof` before growing either.

## Conventions

The full architecture and rules are in [CLAUDE.md](CLAUDE.md), which is written
for both people and coding agents. The short version:

- The root package depends only on `gqlparser/v2` and the standard library.
- No reflection on the request path.
- Request-scoped extension state goes in through `OperationContext.GetOrSet`,
  never `Get` then `Set`. `lint/` ships an analyzer that catches the
  difference; `-race` does not.
- Comments explain why, not what. English only.
- Commit messages: imperative, lower-case type prefix (`feat:`, `fix:`,
  `test:`, `refactor:`, `docs:`).

## Generated code

`examples/blog` is generated. If you change its SDL, regenerate and commit the
result:

```sh
cd examples/blog && go generate
```

CI fails if the checked-in output differs from what the generator produces.
