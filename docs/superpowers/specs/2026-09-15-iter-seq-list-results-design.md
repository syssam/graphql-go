# iter.Seq list results

A field bound to a list may return `iter.Seq[E]` or `iter.Seq[*E]` wherever it
can return `[]E` or `[]*E` today. The resolver stops building a slice it only
ever hands to the writer once.

## Why

The engine writes JSON straight into a pooled buffer. A list resolver that
returns `[]*Post` builds a slice whose only purpose is to be walked once and
discarded. When the source is a database cursor or a paginated API the slice
is pure overhead, and it is proportional to the page size.

The traversal contract already fits. `registry.traversers` is
`map[reflect.Type]func(any, func(int, any) bool)` — push-based, the same shape
as `iter.Seq2[int, any]`. Nothing in the executor pulls from a list; it hands
the traverser a yield function, exactly as range-over-func does.

This is a position gqlgen cannot reach: it builds a `graphql.Marshaler` tree
per field per request, so an element must exist as a value before it can be
marshalled.

## The constraint that bounds the feature

**Concurrent lists cannot stream, and must not try.**

`WaveCoordinator.ready` (`wave.go:138`) will not flush a DataLoader batch until
`begun >= announced`, and `writeListConcurrent` announces the wave with
`pushWave(ctx, len(elems))`. The count must be exact before any sibling task
starts:

- announce too few and the wave flushes early, degrading batching to N+1 —
  the bug class that took four runs in forty to reproduce and needed
  `lint/gqlvet` to find;
- announce too many and `begun` never reaches `announced`, so parked loaders
  are never released. That is a hang, not a slow path.

An iterator has no length until it is drained, so a concurrent list must drain
first. This is not an implementation gap to engineer around later; it is the
batching contract. **No part of this design may make `pushWave` take an
estimate.**

The existing code already drains into `[]any` before announcing, so the
announced count stays exact with a seq traverser. It did need one change:
when the drained list turns out to be shorter than two elements the
concurrent path declines it, and the sequential path used to traverse the
value a second time — which a single-pass seq answers with nothing. So
`writeListConcurrent` now hands the drained elements back to its caller, which
writes them instead of re-traversing.

What each list kind gets:

| element type | path | outcome |
|---|---|---|
| no resolver fields beneath it | `writeList` | streams; no backing slice exists at all |
| any `Resolve` field beneath it | `writeListConcurrent` | drained into `[]any`; the caller's slice is still saved |

So the guaranteed win is "the resolver does not allocate the slice". Full
streaming is the sequential case only, and the documentation must say so
rather than implying lists are lazy in general.

## Scope

In scope: composite lists — objects, interfaces, unions — at a single list
level, for both `Resolve`/`ResolveArgs` and `Field`/`FieldArgs`.

Out of scope, deliberately:

- **Leaf lists.** `[String!]!` is written by a leaf writer registered for the
  whole Go type (`object.go:201`), not by a traverser, so it is a separate
  mechanism. It is also the weakest case: a `tags []string` struct field
  already owns its slice, so laziness saves nothing.
- **Nested seqs** (`iter.Seq[iter.Seq[*Post]]`). One level matches how `[]E`
  and `[]*E` register today; deeper nesting stays on the documented
  reflection traverser.
- **`iter.Seq2[R, error]`.** A cursor that fails mid-iteration is a real case,
  but per-element failure entangles `errNonNull` and `indexedError`, which are
  the most delicate paths in the executor. If it is wanted it gets its own
  design.

## Design

No new public API. `iter.Seq[E]` and `iter.Seq[*E]` become accepted result
shapes in the constructors that already accept `[]E` and `[]*E`, the same way
those two are both accepted now.

Four change points, all in existing machinery:

1. **`registerObjectShapes`** (`registry.go:262`) also registers, for
   `iter.Seq[E]` and `iter.Seq[*E]`: a `traversers` entry that calls the seq
   and counts to synthesise the index the executor expects, and a `nilChecks`
   entry. `registerAbstractShapes` gets the same for `iter.Seq[T]`.

2. **`shapeFor`** (`registry.go:335`) learns to derive a seq's element type.
   Today it calls `t.Elem()` only when the kind is `Slice` or `Array` and
   otherwise falls back to an untyped element shape, which would silently lose
   `toPtr` and any nested traverser.

3. **`checkOutputCompositeShape`** (`validate.go:64`) counts a seq as one list
   level. Today it requires `Slice` or `Array` per level and would reject a
   seq with "has fewer list levels than", which is both wrong and confusing.

4. **Nil detection.** `shapeFor`'s fallback switch (`registry.go:331`) lists
   `Pointer`, `Slice`, `Interface`, `Map` — not `Func`. A nil `iter.Seq` would
   therefore not read as null and would be **called**, panicking. The explicit
   `nilChecks` entries from (1) cover registered types; the fallback gains
   `Func` so an unregistered seq nulls rather than panics.

Seq detection is structural, not by name: kind `Func`, one parameter, no
results, where that parameter is itself a func taking one value and returning
`bool`. The element type is `t.In(0).In(0)`. Matching on `iter.Seq`'s package
path would reject a user's own equivalent for no reason.

## Error handling

Unchanged. A resolver still returns `(iter.Seq[R], error)`; the error is for
the list as a whole, exactly as `([]R, error)` is today. Element failures are
already handled by `writeList`: a failed nullable element rewinds to its mark
and writes null, a failed non-null element rewinds the whole list and fails
it. Yield-based traversal returns `false` to stop early, which the existing
traverser contract already uses.

A panic inside a seq surfaces through the same recovery as a panic in a
resolver, because the seq is called during `writeValue`.

## Testing

- Parity: for a fixture list, `[]*E` and `iter.Seq[*E]` must produce
  byte-identical JSON, including null bubbling of a failed non-null element
  and index-carrying error paths.
- A nil seq writes null and does not panic.
- Early termination: a seq whose consumer stops (non-null element failure)
  must not continue yielding.
- Concurrency: a list whose element has a `Resolve` field must still batch
  through the loader — assert the loader sees one batch, not N. Under `-race`,
  per the project gate.
- Allocation: a benchmark asserting the sequential path allocates fewer bytes
  than the slice equivalent, since "no backing slice" is the whole claim.

## What would make this wrong

If a later change makes `pushWave` accept an estimated count in order to
stream concurrent lists, this design is void and DataLoader batching is
broken. That is the one line not to cross.
