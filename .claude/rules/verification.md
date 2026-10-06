---
paths:
  - "scripts/**"
  - ".github/**"
  - ".golangci.yml"
  - "staticcheck.conf"
  - "**/*_bench_test.go"
  - "alloc_baseline_test.go"
  - "bench_*_test.go"
  - "docs/alloc-baseline.txt"
  - "benchmarks/**"
  - "compare/**"
  - "lint/**"
---

# Gates, coverage and measurement: the full record

The short version is in the root CLAUDE.md. This is the evidence behind it.

**Watch for measurements that succeed while covering less than they look.** A red build is
easy; a command that used to be complete, quietly stopped being, and still prints success is
not. The instances so far, all found by asking what a passing result would look
like if the thing under test were broken:

- `go test ./...` printed `ok` for every package it knew about, and had silently stopped
  reaching three modules as they were added.
- CI's fuzz job discovered its targets by grepping the **root package's** `*_test.go`, so
  `internal/jsonw`'s `FuzzString` and `FuzzKey` — the JSON string escaping, where a bug is a
  corrupted response — had never run. 15s each on first contact took 745k and 2.0M executions
  and added 28 and 112 corpus entries, which is what never having been explored looks like.
  Discovery is `go test -list` over `go list ./...` now.
- CI ran `scripts/gate.sh`, which **skips** a module whose fixtures are missing, and
  `compare/` is generated and gitignored. So CI printed `compare skipped` and exited 0, and
  `TestEnginesAgree` — the correctness cross-check against gqlgen — had never run there. CI
  generates at `-n 25` (6s) and passes `GATE_REQUIRE_ALL=1`, which turns a skip into a
  failure, so dropping that step cannot quietly shrink the gate again.
- CI's lint job had no `working-directory`, so `golangci-lint` linted the root module and
  nothing else. `benchmarks/` was carrying 18 findings under the repository's own policy and
  `compare/` 4, neither ever asked; `lint/` was clean. Fixed: the job now walks every module
  the way `gate.sh` does, the findings are fixed, and `noctx` is excluded for
  `benchmarks/buildbench/` with its reason -- a harness shelling out to `go build` has no
  request to cancel, and a `context.Background()` threaded through to satisfy the linter
  would cancel nothing while reading as though it could.
- A locally installed `govulncheck` built with an older Go than `go.mod` names **exits 0
  having analyzed nothing**. Every package reports "requires newer Go version go1.27
  (application built with go1.26)" on stderr, the findings list is empty, and the exit code
  says pass. It was read here as a green gate. `govulncheck -version` prints the Go it was
  built with; check that before believing a clean scan, and build it from source as CI does.
- A subscription leak test passed against a deliberately broken release path, because a
  second redundant path still freed everything. It only failed when both were broken.
- A fan-out benchmark reported 45ns per subscriber because it timed a `publish` that drops
  into a full buffer. Counting receipts put it at 4us — the events it was "measuring" had
  reached nobody.
- Generated code that read correctly and did not compile, twice: a method taking the
  generated args struct (import cycle) and `ID!` bound to a `string` field. Both were found
  by compiling the output, neither by reading it.
- `go test -cover ./...` reported `internal/httpreq` at **12.8%**. It is at **89.7%**, with
  no uncovered function: coverage is attributed per package, and httpreq is driven across
  package boundaries by the six transports. A package that owns the CSRF and body-limit
  rules looking untested is the wrong signal in the wrong place. Measure it the way it is
  used, and expect the same distortion for any `internal/` package with no direct caller:

  ```sh
  go test -short -coverpkg=./internal/httpreq/... ./internal/httpreq/... ./transport/...
  ```

  `internal/gqlwsproto` is the same story with its own tests present: 77.0% alone, **91.2%**
  measured with the transports that drive it, and `finishWithErrors` reads as 0% there while
  being fully exercised by them. A protocol state machine that looks two-thirds tested is
  the wrong signal to act on.

  ```sh
  go test -short -coverpkg=./internal/gqlwsproto/...     ./internal/gqlwsproto/... ./transport/gqlws/... ./transport/gqlfiber/... ./transport/gqlecho/...
  ```

  `-short` distorts in the other direction: it puts `codegen` at 62.9% when the full run,
  which compiles the generated output, measures 92.6%.
- **A coverage reading of 0.0% on a function that is fully tested.** `ext/trusted.Store.Set`
  is `func (s *Store) Set(string, string) {}` -- an empty body, so 0 of 0 statements, which
  `go tool cover` reports as 0.0%. It has a test, added after a review found it had none, and
  a later pass nearly re-added a second one on the strength of the number. Read a suspicious
  0.0% against the function body before acting on it.
- **A test that measured the drain instead of the traversal.** A guarded-list test put a
  `Resolve` field under the list to make "this element was reached" observable. That made the
  list deeply schedulable, and `WithMaxConcurrency` defaults to `GOMAXPROCS*4`, so
  `writeList` drained it first through `drainList` -- whose callback always returns true,
  because draining has to see every element. The test passed, the counter counted the drain,
  and the early-exit it was written for never ran. The clue was `-race`, which reported a data
  race on the test's own counter: the fields were running concurrently, which they could not
  have been on the path the test believed it was on.
- **A break that did not compile.** Deleting a suffix loop left an unused import and an unused
  variable, so `go build` failed and the test was never run. An invalid break proves nothing
  and reads exactly like a test that did not catch the change. Build before believing either
  outcome.
- **A verification against something outside the repository.** `extraQualifier`'s
  reuse-an-existing-import branch was the fix for a real bug, confirmed by regenerating
  against a consumer schema that is not in this tree -- so nothing here reproduced it, and
  `go test ./codegen` said the branch was uncovered. If a fix was proved against an external
  artefact, it does not have a test.
- **A reverse edit that lands on the wrong occurrence.** Three times: `plan.go`'s `get` and
  `put` carry the same guard line and a single-shot replace hit `get`; two `extraQualifier`
  loops came back swapped; an `ext/apq` restore left a stray blank line. Every one was found
  by `git diff`, none by a test. **Check the diff after undoing a break, not just that the
  suite is green.**
- **A guard that could not see what it guarded against.** `scripts/ci-local.sh` copies the
  tree into a Linux container as a tar archive, and macOS's tar writes a `._name` beside every
  file with extended attributes; unpacked on Linux those are files, the generated packages embed
  `schema/*.graphql`, and the first run failed every schema-building test. The fix was
  `COPYFILE_DISABLE=1`, with a check that the archive held no `._` entry -- made by listing it
  with the same tar, which folds those entries into the next one when it lists. Remove the fix
  and the check stayed green. It runs where the archive is unpacked now, and was confirmed by
  removing the fix.
- **A cached pass.** `scripts/ci-local.sh` keeps Go's build cache between runs, as CI does,
  and Go's cache of test results is in it. The allocation job ran `go test -run
  TestAllocationBaseline -v .` as the workflow writes it, with no `-count=1`, and on a run
  where the root package had not changed it answered in one second: `ok (cached)`. That was a
  true result and not a test that ran; it was noticed only because the same job had taken
  seventeen seconds the run before. The mirror and the workflow both pass `-count=1` there
  now. The gate and the PostgreSQL job always did.
- **A break that hangs.** Removing the loader's key dedup leaves a second waiter on a key the
  queue carries once, and the test ran to the 600-second default timeout. Run a break that
  could deadlock with a short `-timeout`; the gqlwsproto and wave breaks after that were
  caught in 30.
- **The analyzer's own gate, three ways.** CI ran `gqlvet ./...` from the root, so it reached
  one module of five. The test named for the type name gqlvet ships with assigned that name
  itself, so editing the default, or renaming `OperationContext`, left it green and gqlvet
  silent on a planted pair. And nothing required gqlvet ever to report anything: silence is
  also what a dead analyzer says. It loops over every `go.mod` now, a test runs the shipped
  default against the real type, and CI requires exit 3 on `scripts/testdata/gqlvet`.
- **Configuration and shell that read as on.** Listing `revive.rules` replaces revive's
  defaults, so `exported` had never run while an exclusion and its comment described it as
  running. `go test -run Name` matching nothing is `ok [no tests to run]`, exit 0. `for x in
  $(cmd)` runs no times when `cmd` fails, so a package whose tests stopped compiling dropped
  out of the fuzz job in silence. `git diff --quiet` does not see untracked files, so a
  generated file never committed was not stale. Each is asserted now: the PASS line, a listing
  per package that declares a target, `git status --porcelain`.
- **CI weaker than its mirror.** The mirror proved the backoffice suite ran on PostgreSQL and
  the workflow did not, though the mirror exists to fail when CI would. A step hardened in one
  belongs in the other; `BACKOFFICE_REQUIRE_POSTGRES` makes the SQLite fallback a failure in
  both.
- **A snapshot that pinned names.** `docs/public-api.txt` held `var X` and `const Y` and no
  more, so an exported variable's type and a constant's value could change under an unchanged
  line. It renders the declared type and the value, following a re-export into the internal
  package it comes from.

- `TestPoolCapCoversALargeSchemasIntrospection` passed for the whole life of the 8 MiB pool
  cap and was guarding nothing, because it asserted a size **derived** from the thing it was
  meant to check. The cap had been sized as "introspection to roughly 8 000 types" by
  multiplying a type count by the 0.79 KB per type a synthetic schema showed, and the test
  grew a buffer to 4.3 MB from the same arithmetic. Introspection response size follows type
  *width*: the real consumer schema is 1.25 KB per type, so its 5 516 types answer in
  6.56 MB, reach 8.05 MiB of capacity and were dropped by the pool on every request --
  71.3 MB/op with no reuse between requests, against 31.5 once the cap fits. A test whose
  input comes out of the same estimate as the constant it checks will always agree with it.
  It now pins the response measured on the real schema, and fails at 8 MiB naming the
  capacity reached.

**`gqlvet` matches on the key's value, not on anyone's spelling.** It compared the receiver's
text, so an alias, a field holding the context or a second `OperationFrom` hid the pair; it
rendered every composite literal as `?`, so two different `struct{}` keys were one; and it
compared the key's text, so a constant, a variable holding the key and the literal were three.
It now also follows a type alias, an embedding struct, method values and an interface variable
assigned in the same function, and places a call inside a named closure where the closure is
used. **It sees one function at a time**: a `Get` in one helper and the `Set` in another is the
same race and passes clean. `lint/testdata/src/b` has a `// want` line per variant and the
correct code beside each; `TestContextRaceOnTheRealContext` is the one that fails when the type
is renamed.

The habit that catches these is breaking the thing on purpose and requiring the test to
fail. If it still passes, the test was agreeing with the code rather than checking it.

**`scripts/ci-local.sh` is CI's Linux leg and its PostgreSQL job, in Docker.** Before it the
PostgreSQL job ran nowhere but CI, and the tests that need a case-sensitive filesystem ran
nowhere on a Mac or a Windows machine. It copies the tree rather than mounting it (a bind mount
keeps the host's case rules), builds its tools inside the container, waits for PostgreSQL over
TCP, and **requires proof the suite ran on PostgreSQL**: with `BACKOFFICE_POSTGRES` unset or
misspelled the same tests pass on SQLite, so the server has to have logged the tests'
`CREATE DATABASE`s and no test may skip (confirmed by misspelling the variable: 44 passed, 0
databases, reported as a failure). Versions are read from `go.mod` and the workflow. First full
run, 2026-10-02: every job passed; five `codegen` tests skip on Linux and two others on macOS,
so between the two every one runs. The `host` job is the workflow's test job run natively on a
fresh copy, which makes the macOS leg (and, under Git Bash, the Windows one) a command rather
than four steps remembered; it fails on an unformatted file in under a second, which is how it
was checked.

**Running `govulncheck` in `golang:1.27` settles the version question.** The trap above is a
locally installed binary built with the wrong Go; a container pinned to the version in
`go.mod` cannot have it, and `govulncheck -version` prints the Go it was built with so the
scan says so itself:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.27 bash -c '
  go install golang.org/x/vuln/cmd/govulncheck@latest
  govulncheck -version
  (cd compare && go run gen.go -n 25)
  for m in $(find . -name go.mod -not -path "./ref/*" | sed "s|/go.mod$||"); do (cd $m && govulncheck ./...); done'
```

Run on 2026-09-24 over the root, `benchmarks/` and `lint/` (scanner v1.8.0, DB 2026-09-16, Go
1.27.1): **0 affecting each.**
Read the "modules you require" tail as well as the headline -- it found two in
`golang.org/x/mod@v0.39.0` under `lint/` that were fixed in v0.40.0 and uncalled, so nothing
was affected and the bump was still free. What is left is
`golang.org/x/crypto@v0.57.0`/GO-2026-5932 in the root and `benchmarks/`, **fixed in N/A**:
there is no version to move to, this code does not call it, and it will keep appearing until
upstream ships one.

**Fuzz targets are discovered, not listed.** `go test -list 'Fuzz.*'` over `go list ./...`
finds seven: `FuzzRequirementDirectiveLiteral`, `FuzzExecute`, `FuzzFieldsCanMerge` and
`FuzzOperationMetrics` in the root, `FuzzObject` in `internal/jsonr`, `FuzzString` and
`FuzzKey` in `internal/jsonw`. A hand-maintained list is how `internal/jsonw` went unfuzzed.
The five that existed on 2026-09-24 ran 45s each on Linux: all pass, ~10.2M executions total,
no crashers.

**Lint is `golangci-lint run ./...`, and it runs in every module.** It did not always: the
CI job had no `working-directory` and `golangci-lint` lints the directory it is run in, so
for a long time it covered the root and reported success over a quarter of the repository --
`benchmarks/` was carrying 18 findings under this very config and `compare/` 4. The job now
finds every other `go.mod` the way `scripts/gate.sh` does, so a fifth module needed no edit.
Run it the same way locally; `./...` from the root still reaches one module of five.
`golangci-lint` refuses to run twice at once ("parallel golangci-lint is running"), so loop
rather than launching them together.

`.golangci.yml` is the policy
and every disabled check there carries its reason; `staticcheck.conf` exists only so a bare
`staticcheck ./...` agrees with it instead of burying you in findings CI does not report. Two
things that waste time if you do not know them: a `staticcheck` binary older than the Go in
`go.mod` fails with `export data version 4 is greater than maximum supported version 2` and
reports nothing useful, and listing a gocritic check under `disabled-checks` that is already
off by default makes golangci-lint warn on every run. The `exported` rule is off for
`examples/` on purpose — the examples carry a comment wherever there is a why, and the rule
would otherwise demand "User returns the user" twenty-six times, which is the code-narrating
comment the conventions in the root CLAUDE.md exist to keep out.

**`-race` is not optional.** The DataLoader N+1 race was caught 4 times in 40 runs with
`-race` and 0 times in 40 without it: the detector perturbs scheduling enough to hit the
interleaving. Dropping `-race` to save time silently disables the only thing that finds
this bug class. `testing/synctest` does not help here and makes it worse — its
deterministic scheduling hid the same bug in 20 of 20 runs. Use synctest for tests that
would otherwise wait on real time (see `wave_test.go`), not to find races.

Comparing performance between two versions goes through `benchstat`, not by eye:

```sh
go test -count=10 -run '^$' -bench . -benchmem > old.txt   # before
go test -count=10 -run '^$' -bench . -benchmem > new.txt   # after
benchstat old.txt new.txt                                  # golang.org/x/perf/cmd/benchstat
```

Single samples on this codebase have been wrong by 20-77% when the machine was warm.

**Two sequential runs measure the machine as much as the change.** Contention that
arrives between `old.txt` and `new.txt` lands entirely on one side. On a busy machine —
several agent sessions and their language servers — build two test binaries and alternate
them, so every unit of contention is shared:

```sh
git worktree add --detach /tmp/base HEAD && cd /tmp/base   # then edit back to "before"
go test -c -o /tmp/before.exe .                            # and from the main tree:
go test -c -o /tmp/after.exe .
for i in $(seq 1 12); do
  /tmp/before.exe -test.run xxx -test.bench . -test.benchmem -test.count=1 >> before.txt
  /tmp/after.exe  -test.run xxx -test.bench . -test.benchmem -test.count=1 >> after.txt
done
benchstat before.txt after.txt
```

This is not the same as more runs. A sequential comparison of a change that does strictly
less work reported it 50% *slower* here; interleaved, the same change came out
-3.76% (p=0.040, n=12) with allocations equal sample for sample, while per-sample spread
stayed at +/-25% because the machine really was that noisy. The spread survives; the
comparison does not have to.
