---
name: bench-compare
description: Compare benchmark performance between the committed code (or a given ref) and the working tree with interleaved runs of two test binaries and benchstat. Use whenever a change is claimed to be faster, slower or allocation-neutral, or before touching a hot-path struct.
argument-hint: "[benchmark regex] [base ref, default HEAD] [package, default .]"
---

This machine is noisy. Single samples have been wrong by 20-77%, and two sequential runs
measure the machine as much as the change: a strictly-cheaper change once read as 50% slower.
So build two binaries and **alternate** them, and every unit of contention lands on both sides.
`-count=N` does not interleave.

Arguments: `$ARGUMENTS`. Defaults: bench regex `.`, base `HEAD`, package `.`.

```sh
W=$(mktemp -d)
git worktree add --detach "$W/base" <base>          # the "before" tree
(cd "$W/base" && go test -c -o "$W/before.exe" <pkg>)
go test -c -o "$W/after.exe" <pkg>                  # the working tree
for i in $(seq 1 12); do
  "$W/before.exe" -test.run xxx -test.bench '<regex>' -test.benchmem -test.count=1 >> "$W/before.txt"
  "$W/after.exe"  -test.run xxx -test.bench '<regex>' -test.benchmem -test.count=1 >> "$W/after.txt"
done
benchstat "$W/before.txt" "$W/after.txt"
git worktree remove --force "$W/base"
```

The working tree may hold uncommitted changes, so the base has to be a worktree, never a
checkout or stash of the main tree. Run the benchmark package from the directory that owns it
(for example `benchmarks/` is its own module).

Report the benchstat table as it is printed. Then:
- Quote allocs/op and B/op as the reliable figures; they are deterministic.
- Quote sec/op only with its p-value, and call a result with p > 0.05 "not distinguishable",
  not "no change" and not "faster".
- If the per-sample spread is wide (±10% or more), say so. That spread is the machine's.
- Do not argue from a CPU profile. `overLimit` looks hot in a profile and is not.

For a struct-size change on `execState`, `OperationContext` or `planField`, also check
`unsafe.Sizeof` against the size classes pinned by `TestStructSizes`.
