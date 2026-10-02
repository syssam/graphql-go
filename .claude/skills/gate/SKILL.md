---
name: gate
description: Run every gate this repository has (all five modules, API surface, allocation baseline, lint, gqlvet) and report the evidence. Use before claiming a change is clean, before committing, or when asked whether the tree passes.
argument-hint: "[-short] [fast]"
---

Run the gates below and report each one's result as evidence: the command, pass or fail, and
the failing output if it failed. Never summarise a gate you did not run as passing. If one was
skipped, say so and why.

`$ARGUMENTS` may contain `-short` (skip the slow subprocess and load tests) or `fast` (steps 1-3
only).

1. **Every module**, not only the root. `go test ./...` reaches one module of five.
   ```sh
   [ -d compare/schema ] || (cd compare && go run gen.go -n 25)
   GATE_REQUIRE_ALL=1 sh scripts/gate.sh      # append -short if requested
   ```
   With `GATE_REQUIRE_ALL=1`, a module that gets skipped fails the gate instead of disappearing
   from it.
2. **Public API surface.** `go test -run TestPublicAPISurface .` If it fails, show the diff. Run
   `-update-api` only when the API change was deliberate, and say that you did.
3. **Formatting.** `gofmt -l $(git ls-files -co --exclude-standard '*.go')` must print nothing.
4. **Allocation baseline**, without `-race`, which changes allocation counts:
   `go test -run TestAllocationBaseline .` More allocations fail the gate; fewer are logged.
   Run `-update-allocs` only for a deliberate improvement, and say that you did.
5. **Lint, in every module.** CI lints only the root:
   ```sh
   for m in . benchmarks compare lint; do (cd $m && echo "== $m" && golangci-lint run ./...); done
   ```
   `benchmarks/` and `compare/` carry known pre-existing findings (18 and 4). Report them as
   pre-existing, and call a finding new only if it is on a line this change touched.
6. **gqlvet** (the `GetOrSet` analyzer):
   ```sh
   T=$(mktemp -d) && (cd lint && go build -o "$T/gqlvet" ./cmd/gqlvet) && "$T/gqlvet" ./...
   ```

Run independent steps in parallel where the shell allows. Finish with a table with columns
gate, result and note, and one line: clean, or what blocks it.
