# Bounding response size — design

## Problem

Nothing bounds how large a response may grow. Depth, complexity and cost limit what a
query *asks for*; none limits what resolvers *return*. `{ users { posts { comments { body } } } }`
passes a complexity limit tuned for typical data and still writes hundreds of megabytes when
one tenant has a pathological row count. The executor holds all of it in memory before the
transport sends a byte, and `jsonw.Put` then declines to pool the buffer, so every such
request is a fresh large allocation.

Checking the root writer's length is not enough:

- Concurrent sibling fields and concurrent list elements write into their own pooled
  sub-buffers (`writeFieldsConcurrent`, `writeListConcurrent`) and are only spliced into the
  root writer once every task finishes. The root writer sees none of that memory until then.
- `writeListConcurrent` starts one goroutine per element; the semaphore limits which ones
  hold a slot, not how many run. A 10,000-element list is 10,000 buffers growing at once.
- `execState` is 64 bytes, on a size-class boundary, with no padding left (`actualCost`
  and `cancelled` are 4 bytes each). A counter there costs 16 bytes on every request.

## API

```go
// WithMaxResponseBytes bounds the size of a response's data. Zero means unlimited.
func WithMaxResponseBytes(n int64) ExecutorOption
```

A new error code `CodeResponseTooLarge = "RESPONSE_TOO_LARGE"`.

Zero means unlimited, matching `WithMaxDepth`. The default is decided by measurement in the
plan's first task: if an enabled budget costs nothing distinguishable from disabled in
interleaved `benchstat` runs, the default is 64 MiB; otherwise the default is unlimited and
the option is opt-in. Either way the decision and its numbers are recorded in `CLAUDE.md`.

## Semantics

The limit applies to the `data` bytes of one response: one query or mutation, or one
subscription event. `errors` and `extensions` are not counted (see Out of scope).

When a response exceeds the limit:

- `data` is `null`.
- `errors` is exactly one error: message `response exceeds the maximum size of N bytes`,
  `extensions.code` `RESPONSE_TOO_LARGE`, no path, no locations. Other field errors are
  discarded — they carry paths into data that no longer exists, and there may be many of
  them.
- Execution stops early: every field checkpoint after the budget trips fails without
  resolving, the same way a cancelled context does. Resolvers already running finish.
- A subscription stream continues; only that event's response is replaced.

The final decision is exact: after the root `writeObject` returns, the root writer's length
is compared with the limit, so a response of `N` bytes succeeds and `N+1` fails. What is
approximate is the in-flight bound — how much memory execution holds before it notices.

## Design

### The budget lives on the writer

`jsonw.Writer` is pooled, so a field on it costs no per-request allocation. The root writer
embeds the budget; sub-writers point at their root's.

```go
type budget struct {
	used    atomic.Int64 // bytes reported by every writer sharing this budget
	limit   int64
	chunk   int
	tripped atomic.Bool
}

type Writer struct {
	buf        []byte
	stack      []bool
	pendingKey bool
	own        budget
	budget     *budget // nil: unlimited; &own on a root writer; the root's on a sub-writer
	reported   int     // bytes of buf already added to budget.used
}
```

`jsonw` stays free of engine types; the budget is an unexported detail behind four methods:

```go
func (w *Writer) Limit(n int64)          // root: bound this writer and every writer sharing it
func (w *Writer) ShareLimit(from *Writer) // sub-writer: share from's budget
func (w *Writer) OverLimit() bool         // checkpoint: report growth in chunks, compare
func (w *Writer) TripLimit() bool         // true exactly once per budget
```

`Reset` (and so `Put`) subtracts `reported` from a shared budget and clears `budget`,
`reported` and `own`. A sub-writer returned to the pool therefore gives back what it
reported, so after execution `used` equals what the root writer reported — a writer that
forgot to give back would show as drift in a test, not as a silently wrong limit.

### Reporting in chunks

`OverLimit` does not touch the atomic on every call. It computes `len(buf) - reported`
locally and only when that delta is at least `chunk` in either direction (a rewind shrinks
the buffer) does it `Add` it and update `reported`. It then `Load`s `used` and compares.
`chunk` is `min(4096, max(1, limit/16))`, so a small limit in a test is not hidden behind a
4 KiB reporting granularity.

The in-flight overshoot is bounded by `chunk` per live writer, plus one field's own output,
since a checkpoint runs before a field and not inside a leaf writer. A leaf value is already
in memory in the resolver's result before it is written, so that overshoot is proportional
to data the process already held. This is documented on the option, not hidden.

### Checkpoints in the executor

- `writeFieldValue`, beside the existing `ctx.Err()` check:
  `if w.OverLimit() { st.responseTooLarge(ctx, w); return false }`. With no limit this is
  one nil compare on a writer already in cache.
- `writeFieldsConcurrent` and `writeListConcurrent` call `sub.ShareLimit(w)` after `jsonw.Get`.
- `runOperation` and `runSubscriptionEvent` call `w.Limit(e.maxResponseBytes)` when it is
  non-zero, and after the root `writeObject` compare `w.Len()` with the limit exactly. Over,
  or tripped during execution: rewrite `data` as `null` and replace `st.errs` with the single
  error.

`responseTooLarge` records nothing itself beyond calling `TripLimit`; the one error is
produced at the end, so a trip racing a cancellation cannot produce two top-level errors or
none.

`execState` and `OperationContext` do not change size. `TestStructSizes` logs both and the
branch re-measures them.

## Testing

Each test below is broken on purpose once, and must fail when it is.

1. `jsonw`: `OverLimit` reports in chunks and trips; `ShareLimit` accumulates across writers;
   `Reset`/`Put` gives back exactly what was reported (`used` returns to the root's share);
   a rewind reports a negative delta; `TripLimit` is true once; no budget means `OverLimit`
   is false and costs no atomic.
2. Exact boundary: a query whose data is exactly `N` bytes succeeds with limit `N` and fails
   with `N-1`.
3. Shape of the failure: `data` null, one error, code `RESPONSE_TOO_LARGE`, no path, and
   another field error from the same query is not present.
4. Early stop, sequential: a list of 1,000 objects whose child resolver counts its calls
   stops well short of 1,000 under a small limit.
5. Early stop, concurrent: the same with sibling concurrency, proving sub-writers share the
   budget — with `ShareLimit` removed it resolves every element.
6. Subscription: one oversized event fails alone; the next event succeeds.
7. No limit: behaviour and allocation counts unchanged (`BenchmarkFieldPathBare` 18 allocs).
8. Benchmarks for the default decision, interleaved: disabled vs. enabled at 64 MiB on the
   existing root-package benchmarks.

## Out of scope

- **The `errors` list is not bounded.** A million failing nullable elements produce a
  million errors, all in memory, before the final replacement discards them. That is a
  separate bound with its own semantics and is left as the next item.
- An operation timeout. It is independent of this and is its own change.
- Streaming the response to the transport while executing. The design keeps the
  write-then-send model and its null-bubbling rewinds.
