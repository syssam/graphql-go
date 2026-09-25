---
paths:
  - "schema.go"
  - "source.go"
  - "registry.go"
  - "scalar.go"
  - "enum.go"
  - "object.go"
  - "abstract.go"
  - "input.go"
  - "directive.go"
  - "coerce.go"
---

# Schema build (`NewSchema`)

**Build time — `NewSchema` (`schema.go`, `source.go`, `registry.go`, and the binding files
`scalar.go`, `enum.go`, `object.go`, `abstract.go`, `input.go`, `directive.go`).**
`gqlparser.LoadSchema` parses the SDL, then `SchemaOption`s (`Object`, `Field`, `Resolve`,
`Input`, `Args`, `Enum`, `Scalar`, `Interface`, `Union`, `Directive`, `Query`/`Mutation`/
`Subscription`) register typed adapters in `registry`, keyed by `(GraphQL type name,
reflect.Type)`: leaf writers, decoders, nil checks, list traversers, `shapeInfo`.
`schemaBuilder.build()` then runs six ordered phases — object shells → input decoders →
abstract types → fields → schema directives → coverage validation. Order matters: shells
must exist before fields reference them, inputs before args are composed. **Build errors are ordered by phase, then sorted within it** (`endPhase`): an
unbound type before the fields that needed it, because the first is usually the
cause of the second, but alphabetical inside a phase because several checks
range over `b.ast.Types` and a map gave a different order every run. At forty
unbound types that is already unreadable; at the thousands this library is for,
bound a package at a time, it makes two runs impossible to diff.
`TestBuildErrorsAreOrdered` builds the same schema eight times and requires the
same text, and reports the first line that differs rather than the whole wall.

Go-vs-SDL shape
mismatches are reported here as joined errors, never at request time.

**`Input[T]` honours `json:"-"` as "not on the wire", the same as encoding/json**
(`graphqlNameOf`, `input.go`). It already honoured `graphql:"-"`; for `json:"-"` it fell
through and derived a name from the Go field instead, so an ORM's internal field --
`Predicates []predicate.X` tagged `json:"-"` on every ent WhereInput -- became a required
`predicates` input against a schema that never declared one. 447 of the 12 283 errors the
real schema produced at `NewSchema`. `json:"-,"` stays encoding/json's escape for a field
genuinely named `-`, and has its own test, because reading it as "skip" silently drops a
declared field. `TestInputSkipsJSONDashFields` fails with the exact message the real schema
gave.

**A build error renders the first 20 and says how many there are** (`buildError`,
`builderror.go`). It keeps `errors.Join`'s `Unwrap() []error` contract, so `errors.Is`,
`errors.As` and a caller walking the list are unchanged -- only the rendering stops. A real
schema produced 33 924 on first contact, one per line, which no terminal and no reader gets
through, and the first twenty were enough to act on every time because the list is sorted.
**It is deliberately not a summary by category**: the two categories that made that list
enormous are now caught earlier and more specifically -- gqlc names every unbound scalar
while generating, and an exported-name bug accounted for 342 -- so classifying what remains
would be machinery for a wall that no longer forms. `TestBuildErrorsAreOrdered` walks
`Unwrap()` rather than splitting the text, because splitting it would check the sample.

**`ZeroForNull()` lets a Go type that cannot be null back a nullable input position**, and it
is opt-in per binding for a reason. A `bool` field cannot tell an omitted flag from a false
one, and for a PATCH-style input those are different requests -- that is what `Omittable`
exists for. The case it was added for is the opposite on purpose: entgql emits
`VariantIDIsNil bool` under `variantIdIsNil: Boolean` and reads false as "apply no
predicate", so absent, null and false really are one value; 11 035 such fields on one real
schema, in SDL its author does not hand-write. `codegen.Config.ZeroForNullInputs` emits it on
every generated `Input`, because a schema that needs it needs it everywhere.

**`ZeroForNull` and `Omittable` on one field are not a contradiction.** They read like
opposites -- one says absent, null and false are the same value, the other exists to keep them
apart -- and the combination has the only meaning that loses nothing: an explicit null is
answered with the zero value like any other `ZeroForNull` field, and `IsSet` still separates
that from a field the client never sent. `autoSetter` reaches it through
`Omittable.assign(nil, true)`, which no other path calls
(`TestZeroForNullKeepsOmittableAbleToSeeAbsence`).

**It is two halves and both are load-bearing.** Relaxing `checkInputShape` only gets the
binding built; the decoder for a type that cannot be null still refuses one, so `autoSetter`
answers an explicit null with the zero value *before* the decoder sees it. Breaking either
half alone fails `TestZeroForNullAcceptsAValueAtANullablePosition` -- the first with "cannot
represent null", the second with "null value for non-null type". It applies to derived fields
only: a field declared with `InputField` carries its own setter, so its Go type is already
the author's choice.

**A serialization tag that names nothing in the schema falls back to the Go field name**
(`autoInputFields`, `input.go`). An ORM writes `RoleID int64 \`json:"role_id"\`` because
snake_case is its wire format with the database; the SDL declares `roleId` because camelCase
is GraphQL's. Before the fallback that was two build errors per field -- "roleId has no
binding" and "role_id is not defined in the schema" -- and **nothing could be done about it
from outside**, because `Input` refuses a second binding for one type name and there is no
sound way to relax that: an input object decodes a map into exactly one Go type, so unlike
`Enum` and `Object` its registry key cannot carry a `reflect.Type`. The fallback can only turn
an error into a binding: a tag naming a field the schema does not declare is rejected today,
so nothing that builds now changes. A tag that *does* name a declared field always wins, and
the fallback never takes a name another field's tag claimed, or two fields could quietly swap
(`TestInputTagWinsOverTheGoFieldName`). It applies to `Input` only -- `Args` arguments are not
known until the field that uses them is resolved, so `declared` is nil there.

**A `Directive` binding whose directive can never wrap a field is a build error.**
`DirectiveArgs` applies `FIELD_DEFINITION` and `OBJECT`; a directive declared on neither --
`directive @audit on ARGUMENT_DEFINITION` -- can be bound, and the binding is dead. That was
a `slog.Debug` line, which is off by default, so the author wrote a wrapper, the schema built,
and nothing said the wrapper never runs. It is the fail-open `authz.md` names for the
requirement directives, and the adjacent case -- a binding for a directive the SDL does not
declare at all -- was already an error. The check fires only when **no** declared location is
usable, so a directive with one usable location among several still builds, and the message
lists the declared locations rather than making the author guess which two are applied
(`TestADirectiveBindingThatCanNeverWrapIsABuildError`).

**Schema build scales with the width of the widest type, not the type count.** 4800 types build
in 78 ms and retain 33 MB; narrowing that schema's 4800-field Query root to 100 fields takes the
identical type set to 34 ms, because gqlparser validates a k-field type in O(k^2).

**On the real schema `NewSchema` costs 340 ms and about 130 MB, and 40% of it is gqlparser.**
5 516 types over 828 SDL files, 7.32 MB of text: `gqlparser.LoadSchema` alone is 130 ms and
retains 58.7 MB, and the whole call -- that plus registering roughly 2 000 generated options
and running the six phases -- is 340 ms and +130 MB of heap. It is paid once, at start-up,
and the first request after it is immediate (`{ __typename }` is under a millisecond). Neither
half is worth optimizing before something measures start-up as a problem; what the split is
for is knowing that half of any such attempt would have to happen in a dependency.

**`Time(name)` is gqlgen's wire format with two of its choices refused** (`scalar.go`). Output
is `time.RFC3339Nano` with the value's own offset, byte for byte what gqlgen writes, so a
service moving over does not change what its clients parse. gqlgen writes the zero time as
null, which is an error at `Time!` the value did not cause; here it is
`0001-01-01T00:00:00Z`, and an empty column is a `*time.Time`. gqlgen reads `""` as the zero
time and `2006-01-02 15:04:05` as UTC; both are refused. A year outside 0-9999 is a field
error rather than a string no RFC 3339 reader accepts. `TestTimeIsRFC3339` fails with output
forced to UTC, `""` accepted, or the year check removed.
