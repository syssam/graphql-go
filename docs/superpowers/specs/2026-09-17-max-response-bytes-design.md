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
plan's last task: if an enabled budget costs nothing distinguishable from disabled in
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
  them. The error still passes through the `ErrorPresenter`.
- Execution stops early: every field checkpoint after the budget is exceeded fails without
  resolving, the way a cancelled context does. Resolvers already running finish.
- A subscription stream continues; only that event's response is replaced.

The final decision is exact for an execution that never passed the limit: after the root
`writeObject` returns, the root writer's length is compared with the limit, so data of `N`
bytes succeeds with limit `N` and fails with `N-1`. Once a checkpoint has seen the limit
passed the response is rejected regardless, even if null bubbling later rewound those bytes
away, because every field after that point was cut short and the data is no longer the
query's answer. What is approximate is the in-flight bound — how much memory execution
holds before it notices.

## Design

### The budget lives on the writer

`jsonw.Writer` is pooled, so a field on it costs no per-request allocation. The root writer
embeds the budget; sub-writers point at their root's.

```go
type budget struct {
	used     atomic.Int64 // bytes reported by every writer sharing this budget
	limit    int64
	exceeded atomic.Bool
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

`jsonw` stays free of engine types; the budget is an unexported detail behind five methods:

```go
func (w *Writer) Limit(n int64)           // root: bound this writer and every writer sharing it
func (w *Writer) ShareLimit(from *Writer) // sub-writer: share from's budget
func (w *Writer) OverLimit() bool         // checkpoint: report growth, compare, latch exceeded
func (w *Writer) LimitExceeded() bool     // whether any checkpoint on this budget went over
func (w *Writer) Splice(sub *Writer)      // write sub's bytes and take over what sub reported
```

`OverLimit` is written so the unlimited case inlines to one nil compare; the rest lives in a
separate function.

`Reset` (and so `Put`) subtracts `reported` from a shared budget and clears `budget`,
`reported` and `own`. A sub-writer returned to the pool therefore gives back what it
reported, so after execution `used` equals what the root writer reported — a writer that
forgot to give back shows as drift in a test, not as a silently wrong limit. `Put` resets
even a buffer it declines to pool (over `maxPooledCap`), or a large sub-writer would never
give back.

**Splicing moves the count, it does not copy it.** The executor puts sub-writers back only
after every sibling is spliced, so a parent checkpoint between the splice and that `Put` — a
pure field after two concurrent ones — would count the same bytes a second time and latch
`exceeded` on a response that fits. `Splice` writes the sub-writer's bytes and adds its
`reported` to the parent's, zeroing the sub-writer's, so `used` is unchanged and each byte is
counted by exactly one writer. The first version of this design used `Raw` there; review
reproduced a response rejected at about half its real size, depending only on field order.

### Reporting on every checkpoint

`OverLimit` adds `len(buf) - reported` to `used` whenever it is non-zero (negative after a
rewind), updates `reported`, and compares `used` with the limit. It does not batch.
Batching into fixed-size blocks was considered and rejected: a writer would only report
after growing a whole block, and a concurrent list element's buffer is typically a few
hundred bytes, so ten thousand small elements would never report at all — the exact case
this exists for. The cost is one `atomic.Add` and one `Load` per field when a limit is set,
which the default decision measures.

The in-flight overshoot is bounded by one field's output per live writer: a checkpoint runs
before a field, not inside a leaf writer, and a writer's last field is only seen once its
buffer is spliced into its parent and the parent reaches its next checkpoint. A leaf value is
already in memory in the resolver's result before it is written, so that overshoot is
proportional to data the process already held. This is documented on the option.

### Checkpoints in the executor

- `writeFieldValue`, beside the existing `ctx.Err()` check: `if w.OverLimit() { return false }`.
  With no limit this is one nil compare on a writer already in cache.
- `writeFieldsConcurrent` and `writeListConcurrent` call `sub.ShareLimit(w)` after `jsonw.Get`,
  and splice results with `w.Splice(sub)` rather than `w.Raw(sub.Bytes())`.
- `runOperation` and `runSubscriptionEvent` call `w.Limit(e.maxResponseBytes)` when it is
  non-zero. After the root `writeObject` returns — every task has finished by then — they
  compute `exceeded := w.LimitExceeded() || int64(w.Len()) > limit` *before* any `Reset`
  (which clears the budget). When it holds they rewrite `data` as `null`, set `st.errs` to
  nil and add the single error through `addError`.

The checkpoint records no error itself; the one error is produced at the end, so a trip
racing a cancellation cannot produce two size errors or none.

`execState` and `OperationContext` do not change size. `TestStructSizes` logs both and the
branch re-measures them.

## Testing

Each test below is broken on purpose once, and must fail when it is.

1. `jsonw`: `OverLimit` reports and latches `LimitExceeded`; `ShareLimit` accumulates across
   writers; `Reset` gives back exactly what was reported; a rewind reports a negative delta;
   no budget means `OverLimit` is false.
2. Exact boundary: data of exactly `N` bytes succeeds with limit `N` and fails with `N-1`.
3. Shape of the failure: `data` null, one error, code `RESPONSE_TOO_LARGE`, no path, and
   another field error from the same query is not present.
4. Early stop, sequential: a list of 1,000 objects whose field counts its calls stops well
   short of 1,000 under a small limit.
5. Early stop in sub-writers: two concurrent sibling root fields, each writing such a list
   into its own sub-writer, stop well short of 2,000 calls — with `ShareLimit` removed the
   sub-writers have no budget and every element is written. The same for a list whose
   elements are written one goroutine each, and for one subscription event.
5a. Splice counts once: a pure field after two concurrent fields, with the limit set to the
   exact data size, succeeds; with `Raw` in place of `Splice` it is rejected. A sub-writer
   over `maxPooledCap` gives its bytes back when put.
6. Subscription: one oversized event fails alone; the next event succeeds.
7. No limit: allocation counts unchanged (`BenchmarkFieldPathBare` 18 allocs/op).
8. Benchmarks for the default decision, interleaved: disabled vs. enabled at 64 MiB.

## Out of scope

- **The `errors` list is not bounded.** A million failing nullable elements produce a
  million errors, all in memory, before the final replacement discards them. That is a
  separate bound with its own semantics and is left as the next item.
- An operation timeout. It is independent of this and is its own change.
- Streaming the response to the transport while executing. The design keeps the
  write-then-send model and its null-bubbling rewinds.
