---
paths:
  - "codegen/**"
  - "cmd/gqlc/**"
  - "examples/*/gqlc.yaml"
---

# Codegen (`codegen/`, `cmd/gqlc`)

**Codegen (`codegen/`, `cmd/gqlc`).** SDL-only: it never loads Go packages unless
`AutoBind` asks, and `TestNoPackagesAreLoadedWithoutAutoBind` is what keeps that true --
only the `len(cfg.AutoBind) > 0` branch enforces it, and a regression there makes no
output wrong, it just costs what gqlgen costs. It emits models,
args structs, a `Resolver` interface and bindings that call the same public constructors as
hand-written code. Each group emits a single `generated.go` holding its args, `Resolver`
interface and bindings — one file per package, since that is the unit the compiler rebuilds.
`Config.Manifest` binds types and fields outright instead of inferring them, which is the
mode an external generator wants; it still loads no Go type information, so a method binding
declares its own shape (`Context`, `Error`). Type bindings are folded into `cfg.Models` in
`newBuilder`, so model references, imports and `mapped` keep working unchanged and only
field kinds and group overrides are read from the manifest afterwards. **A method's
arguments are spread into the call, not passed as the generated args struct** — passing the
struct makes the bound type's package import the generated one, which already imports it for
the model, and that is an import cycle. A type in the manifest gets no inference at all: an
unlisted field is a resolver.

**Do not teach `AutoBind` to reach into a nested struct.** ent and the ORMs derived from it
keep relations in an `Edges` field, so `AbTesting.variants` is `AbTesting.Edges.Variants` in
Go, and on a real 5 503-type schema that covers thousands of fields. It was built and
reverted: the ORM also generates `func (m *AbTesting) ParentTest(ctx) (*AbTesting, error)`,
which calls `QueryParentTest().Only(ctx)` when the edge is not loaded. `Edges.ParentTest` is
the raw field, nil whenever nothing eager-loaded it, so binding to it returns null for data
that exists, silently. The resolver count did not improve either — AutoBind already matched
those as methods, which is the right binding — so the change only demoted a correct method
call to a wrong field read. The reasoning is on `discoverFields`.

`Config.AutoBind` discovers bindings from named packages instead of being told them, and
produces a `Manifest` — so discovery is the only new behaviour and everything downstream is
the manifest path. **Only export data is loaded** (`NeedName | NeedTypes | NeedImports |
NeedDeps`): adding `NeedSyntax` or `NeedTypesInfo` would parse every file of every package,
which is the cost this project exists to avoid, so treat either as a regression. Matching is
json tag, then case-insensitive name, then a method whose shape the generator can call;
anything else falls through to `Resolver`, because a resolver method can always be written
where a bad guess is a compile error in code the user did not write. A Go type over the same
basic kind gets a conversion (`graphql.ID(v.ID)`), since an ORM storing an id as a `string`
still answers `ID!` and refusing would send the commonest field in any schema through a
resolver.

**Discovery is two passes, and the order is the whole correctness of it.** Whether a Go field
can answer a GraphQL field is decided by comparing it against the Go type that field needs,
and that type is only known once every type the same pass binds is in the model map. Deciding
fields as the types are discovered measures each one against the model the generator *would*
have written: `Product.owner` is `*ent.Owner` against `*model.Owner`, so **every**
object-valued and every enum-valued field fell through to the `Resolver` however plainly it
was a struct field. Two smaller instances of the same mistake sit either side of it:
`foldModelDirective` returns a new map and the builder kept its old one, so a field typed by
a `@goModel`-bound type was measured the same wrong way; and enums must be folded before
objects for the same reason. `TestDiscoveryBindsObjectValuedFields`,
`TestDirectiveBindingReachesDiscovery` and `TestAutoBindDiscoversEnumConstants` fail if the
fold moves, and each was confirmed by breaking it.

**Three more places read the model map, and the order they read it in was wrong in
all three.** `modelQualifier` asks `modelExprImports` once per type reference and that
scanned every `Models` entry, which with AutoBind holds one per discovered type: the real
schema took 4m58s to generate and 16s once it was memoized behind `setModels`
(`TestModelExprImportsIsComputedOnce`). Fields are read off the *declared* Go type wherever
a declaration exists, not off whichever type discovery found under that name -- two packages
each holding a `PaymentTerms`, one with a `HasEarlyPaymentDiscount` method and one without,
bound the field to a method the declared type does not have
(`TestFieldsComeFromTheDeclaredType`). And the import block is decided by *parsing* the
generated body rather than searching its text: every custom scalar carries "bind it with
graphql.Scalar in NewSchema options" above it, and that comment alone imported the engine
into 1 006 packages that never used it (`TestNoImportIsWrittenForACommentAlone`).

**An enum's Go constants are discovered by value, never by name.** The generator derives a
constant name from the SDL value for an enum whose model it wrote itself, which is sound
because it wrote both sides; for a mapped enum it is a guess, and a wrong guess is a compile
error in a file marked DO NOT EDIT. `constIndex` reads each exported string constant's *value*
-- `AccessPolicyExpectVisible = "VISIBLE"` answers `VISIBLE` exactly, where the derivation
gives `AccessPolicyExpectationVisible`, and `WorkspaceID = "WORKSPACE_ID"` where it gives
`Workspace_id`. `TypeBinding.Values` carries the result. An enum the SDL does *not* map binds
only when **every** value has a constant: a partial discovery would name the found ones in the
generated model package, where they do not exist, so it binds nothing instead
(`TestPartialEnumDiscoveryBindsNothing`). A mapped enum keeps the derived name for a value it
could not find, which will not compile -- but it did not compile before either, and refusing
to generate would stop a schema that works today. Constants are looked for in the packages
`Models` already names as well as the auto-bound ones, because an ORM declares an enum's Go
type beside the entity that uses it and one real schema names about five hundred such
packages; making the author list them a second time is bookkeeping, and getting it wrong is
silent. Those packages are read for their constants only -- nothing binds from them.

**One SDL enum can have two Go types, and discovery finds the second through an input
struct.** The registry is keyed on `(GraphQL type, reflect.Type)`, so both bindings coexist
and each position decodes into the type it actually holds. An ent-derived ORM produces the
pair as a matter of course: `@goModel` binds `AssetDepreciationMethod` to
`velox/asset.DepreciationMethod`, while the filter struct the same ORM generates carries
`velox/assetdepreciation.Method` for the same column. Only the first is bound, so every input
field using the second is a `NewSchema` error the SDL gives no hint of -- and before
`autoInputFields` learned its name fallback there was no way to fix it from outside either.

`altEnumTypes` walks each bound input object's Go struct, matches its fields to the SDL's, and
records a named type that is not the one the enum is bound to; `Manifest.ExtraEnums` carries
the result and the emitter writes a second `graphql.Enum` beside the first. Three parts of it
are load-bearing:

- **Only input objects are searched.** An object field of the unbound type falls to a
  resolver, which the author writes and can convert in. An input field has nowhere to put that
  conversion, which is why this is worth machinery and the object case is not.
- **`modelPkgs` loads the input objects' packages too, not only the enums'.** The second type
  is reachable only through a declared input struct, and a declared type this pass never
  loaded has no fields to look at. The package holding the second type is then loaded by
  `altEnumPkgs`, because nothing in the config names it -- `packages.Load` returns roots, and
  export data for a dependency does not come back as a package of its own.
- **A partial map binds nothing and is reported instead**, the same bar a first binding is
  held to (`TestPartialSecondTypeBindsNothingAndSaysSo`). A missing value would be
  unrepresentable at every position holding that type -- a wrong answer at run time, where the
  unbound type is at least a refused build.

The import is the part that broke first: a package `Config.Models` already names is imported
under that path already, so registering it a second time is a redeclaration in a file marked
DO NOT EDIT. `extraQualifier` settles the qualifier and the import together, once, because
deciding them twice is how a reference and its import disagree.

**A type that encodes itself binds as it is.** entgql and velox generate order fields as a
struct holding a cursor func, with `MarshalGQL` and `UnmarshalGQL` -- gqlgen's contract. They
used to be dropped below, the order input holding one with them, and every edge method taking
an order fell to a hand-written resolver converting a generated string enum back. The engine
now binds such a type directly (`EnumMarshaler`, `ScalarMarshaler`; output of an enum is still
checked against its declared values), and `isGQLMarshaler` makes AutoBind emit that binding
instead of dropping, for scalars as well: velox's `Cursor` no longer needs a hand-written
`graphql.Scalar`. In `examples/veloxfx` it removed the orderBy conversion package and every
connection-edge resolver. `TestAutoBindBindsTypesThatEncodeThemselves` fails with detection or
emission removed.

**An enum whose declared Go type cannot back an enum is modelled instead, and the drop is
reported.** entgql binds an SDL enum to a struct holding a func: not comparable, so
`Enum[T comparable]` cannot take it, and its values are unexported package vars, so nothing
outside that package can name one. 444 of them on the real schema, written into the SDL by
the ORM rather than by hand. Refusing to generate would block the schema; emitting the
binding anyway was 12 959 compile errors. `unbindableTypes` drops the binding and the
generated string enum is what every field, argument and resolver signature then agrees on.
The test is deliberately narrow -- only a Go type whose underlying type is not basic is
refused -- because a named string type with no constants found may simply have them
somewhere this pass did not load. `Config.Notef` carries the report; it is not a log, and
anything that does not meet "a decision the author can act on" belongs in an error or
nowhere.

**A method binds on its result, and its parameters have to be checked too.** `goMethod.fits`
compared the parameter *count* and stopped, while the generator spreads the args struct's
fields into the call as `a.Name` with no conversion -- so every parameter must be exactly the
Go type that argument's field holds. An ORM edge method taking its own `*ent.XOrder`, against
an `XOrder` the generator models itself, emits `cannot use a.OrderBy (*model.XOrder) as
*ent.XOrder`. It was latent until `unbindableTypes` started dropping those containers, which
is the usual shape: the second bug is only reachable once the first is fixed.
`TestAMethodMustMatchItsArgumentTypes` carries a positive control, because "refuse every
method with arguments" would also pass the negative half.

**`goIdent` must produce an *exported* name, and upper-casing the first rune does not.**
A leading underscore has no upper case, so `_lastUpdatedAt` stayed `_lastUpdatedAt` --
unexported, which `Input[T]` skips, so the input could never bind and `NewSchema` refused the
whole schema. 342 fields on the real schema. The generated Go compiled perfectly; only
building the schema found it, which is the third time on this schema that "it compiles" was
read as "it works". A leading underscore is legal in SDL and conventional for a meta field,
so it has to yield a name rather than be rejected. `goIdent` trims the underscores and
prefixes `X`: the prefix stays rather than being dropped so `_x` and `x` on one type do not
both become `X` and collide, which the `graphql` tag would turn into a compile error.
`TestGoIdentExportsALeadingUnderscore` carries positive controls -- `reason`, `id`, `ownerId`
must be untouched -- because "prefix everything" passes the negative half on its own.

**Only an object type takes a pointer; an abstract one does not.** `goType` added `*` for
object, interface and union alike. For an object that is right -- the Go side is a struct.
For an interface or union the Go side is itself an interface, and `*Noder` satisfies nothing;
the executor resolves the concrete type from the dynamic one. An *unmapped* abstract type
becomes `any` and never reaches that line, which is why it went unnoticed until a schema
mapped one -- `interface Node @goModel(model: "pkg.Noder")`, which the ORM writes.
`TestMappedAbstractTypeIsNotAPointer` checks a union as well as an interface, and asserts the
object keeps its pointer, because "stop adding pointers" would pass the negative half.

**An unmapped abstract type whose members are all generated models gets a marker interface**
(`type Node interface{ IsNode() }`, `func (*User) IsNode() {}`) instead of `any`, so a
resolver returning a type the schema does not allow is a compile error rather than a request
error. `hasMarker` decides it once per type and `setModels` clears the memo, because AutoBind
changes which members are mapped. It falls back to `any` when a member is mapped (Go allows a
method only in the type's own package), when the type is itself mapped (it has a Go type
already), and when a member has a field whose Go name is the method. A marker for an interface
that implements another lists the parent's method too, so the value is assignable upward.
gqlparser's `PossibleTypes` includes those implementing *interfaces*, which the first version
treated as unbindable members and so gave `Node` no marker at all. The bindings pass the marker
as `Interface[T]`; a mapped abstract type stays `Interface[any]`, as before -- the executor
resolves by dynamic type either way. On the real schema (default config, no AutoBind, its one
Resolver collision renamed in a scratch copy) it emits 560 markers and 483 methods; `go vet`
is clean over all 803 packages and `NewSchema` builds it with zero errors in 340 ms, run with a
zero-value stub for every Resolver method and a string binding for each of the ten custom
scalars. That is the default-config result, not the AutoBind one recorded above.

**Every generated struct field must carry its `graphql` tag, args structs included.** Input
objects always did, with the reason written above the line: deriving the SDL name back from
the Go name is lossy. Args structs went without one, and it stayed invisible while every
argument name happened to survive the round trip -- `ownerID` does not (it derives to
`ownerId`) and neither does `_lastUpdatedAt`. Fixing `goIdent` is what surfaced it, which is
the usual shape here: the second bug is only reachable once the first is fixed.
`TestArgsStructPinsTheSDLName` asserts the tag on all three shapes and then builds the
schema, because the tag being present is not the claim -- the binding matching is.

**`Config.FieldDirective` honours `@goField(forceResolver: true)`, and is not redundant with
AutoBind refusing a guess.** AutoBind verifies that a struct field exists; the author is
saying the value must be *computed* -- permission filtering, a conversion, a column that
will not be one tomorrow. Binding it to the column compiles and answers the wrong thing,
which is worse than not compiling. 4 567 on the real schema, and **it moved the resolver
count by zero there** -- every one of them was a computed field the ORM type does not have,
so they were resolvers already. It earns its place on the field that is a column and that
the author wants computed anyway; do not quote it as a migration win. Three places decide a field --
the kind, the emitted call and the `Resolver` interface -- and they now share
`modelAnswers`, because honouring it in the call alone emits a `Resolve` for a method the
interface never declares.

**A mapped package's qualifier is chosen per import path and always written as an alias.**
It used to be the path's last element, unaliased, which is right only while a package is
named after its directory, no two mapped paths share a last element, and none ends in
`graphql`. velox breaks the first two at once: entity enums in `.../velox/todo`, mutation
inputs in `.../velox/client/todo` under `package todoclient`. The first generate of
`examples/veloxfx` did not compile, and one of the two `todo` imports had silently won the
qualifier map. `modelExprImports` now assigns qualifiers once (last element, then the last
two joined, then a number, never `graphql` or `context`) and `exprRef` writes references from
the same table. **AutoBind has to compare under that table too**: it matched a Go type to the
generator's expression by rendering the Go side with the package *name*, so the moment the
two could differ `Todo.status` fell through to the Resolver. `goQualifier` renders a mapped
package by its qualifier, and an unmapped one by its path when its name is some mapped
package's qualifier -- otherwise two different `Status` types read alike, which predates this
change. Five breaks were each confirmed to fail
`TestMappedPackagesGetTheirOwnQualifier` or `TestAutoBindComparesTypesUnderTheGeneratedQualifier`:
no alias, no dedupe, `graphql` unreserved, and either half of `goQualifier`.
`TestPathQualifierIsAnUnusedIdentifier` covers the major-version skip and `identFrom`, each
confirmed the same way.

**Groups are registered one by one, like gRPC services, not aggregated in a struct.** The
grouped root used to emit `Resolvers { User user.Resolver; ... }`. A field left unset
compiled, passed `NewSchema` (the zero value is what `ValidateSchema` builds with), and
failed on the first request to reach it. Now the grouped `NewSchema(opts...)` takes each
group's `Bindings(r)`, so a group left out fails the build with `type X has no Object
binding`, and one passed twice fails with `bound more than once`. It also means a large
service has no 800-field struct to fill: each domain contributes its own groups, which with
uber/fx is a value group (`examples/veloxfx`). Groups with no Resolver are registered by
`NewSchema` itself; `ValidateSchema` registers the rest with a nil resolver.
`TestGeneratedTwoGroupsExecutes` and `TestGenerateGroupFuncRegistersPureGroupItself` both
fail if `NewSchema` registers a resolver group by itself.

**Root fields are grouped per field, not with their type.** `Query` is one type, so grouping by
type put every root field in whichever group declared it, even when the SDL spread them over
`extend type Query` in 800 files: all of a large schema's root resolvers in one package. A root
field now goes to the group of the file declaring it (`fieldInGroup`, `eachGroupField`), and
the engine merges the several `graphql.Query(...)` calls back into one root (`Object` merges
by name). velox declares every root field in one shared file, which by-file grouping would
send back to one package, so `Config.RootFieldGroup` can place a field by `ReturnGroup`
instead. `TestRootFieldsAreGroupedByTheirOwnFile` executes one query across three groups, and
it and `TestRootFieldGroupCanFollowTheReturnedType` fail with roots grouped by type again;
the second alone fails with `RootFieldGroup` ignored.
Grouping per field first made generation groups times types -- every group asked every type
about every field, three times, and every import block recomputed the group list -- 1.8 s at
800 groups against 0.3 s flat. `eachGroupField` indexes the fields once and `uniqueGroups` is
memoized (`TestGroupFieldsAndGroupsAreComputedOnce`); with writes made concurrent it is 0.5 s.
A measurement of this once compared five runs against one; time single runs.

**`Config.Scaffold` writes the Resolver methods an implementation lacks, and nothing else.**
Without it every new field meant reading `generated.go` and copying a signature by hand, which
is where gqlgen's stub generation was the better experience. It parses the target package with
`go/parser` only (method names on one type; nothing loaded), appends stubs to
`<group>.resolvers.go`, and opens a new file with `var _ Resolver = (*T)(nil)` so a signature
the SDL changes is a compile error next to the stubs. The signatures come from
`resolverSignatures`, the same function that renders the interface, so the two cannot differ.
It never edits or removes an existing method; a stale one is the compiler's to report. The
first version imported the group package unconditionally and did not compile when the type
was declared elsewhere and no stub took args -- `TestScaffoldKeepsWhatExists` builds that case.
Four breaks (ignore existing methods, overwrite instead of append, not called, no group check)
each fail a `TestScaffold*` test.

One SDL group stays flat in `Output`; two or more become subpackages plus a `Resolvers` struct, with models split the same way (`model/<group>/`) so a one-group edit
does not invalidate every other group's compiled package — except when two groups' input
objects reference each other, which would be an import cycle and falls back to one shared
`model` package (`modelGroupsAcyclic`). Generated files are strings run through `go/format` (not Jennifer), and
content-equal files are not rewritten.

**A schema can be validated with no resolvers at all, and that is why ten bugs survived.**
Binding validation reads the resolver's *type*, never its behaviour -- `Resolve` captures the
function in a closure that build never calls -- so `NewSchema` accepts a zero value. gqlc
emits `ValidateSchema()` for it in both shapes (`var r Resolver` flat, `g.Bindings(nil)` per group).
Before it existed, answering "is this schema bindable?" on the real schema meant faking all
8 724 resolver methods with a 200-line AST walker, which is why nobody had ever asked: the
question cost a day, so the answer was assumed. It costs 340 ms now and reports the same
zero errors. **Reach for it before believing any claim about a schema**, including your own.

**gqlc reports every custom scalar while generating, because it already knows.** A `models:`
entry chooses a scalar's Go type; it does not register a marshaller, so a *mapped* scalar
needs `graphql.Scalar` exactly as an unmapped one does. Believing otherwise made 21 641 of
the real schema's 33 924 errors a surprise -- `Time` was mapped to `time.Time` and still had
no binding. `reportUnboundScalars` now names all ten with their Go types in one line at
generate time. The first version of its own test encoded the same misconception, which is
the measure of how easy it is to hold.

**Codegen against a real schema now compiles, and that is not the same as working.** A real
5 503-type ERP schema -- 828 SDL files, an ent-derived ORM, 3 778 `@goModel` and 4 567
`@goField` directives -- generates in about 9 s into 804 packages that `go build` and
`go vet` clean. At `8e78822` the same schema took 4m58s and produced 29 213 compile errors.
Six bugs stood between those two numbers and every one was found by compiling the output
rather than by reading it; they are described above under AutoBind.

**Then `NewSchema` was run on it and reported 33 924 errors in about 400 ms**, which is
11 839 once two of the three causes were fixed. That is the
whole lesson of this file in one result: a clean `go build` over 804 packages looked like
proof and was not, because the generated bindings are valid Go whether or not they match the
SDL, and only `NewSchema` compares the two. Running it needed a stub for every resolver
methods, which is why it had never been run -- `scratchpad/na/stubgen.go` writes a zero-value
`Stub` into every group package by parsing each `Resolver` interface, and `schemacheck.go`
hands them to `NewSchema` together with a binding for each custom scalar. Generate the two
from different scripts and never let one write the other's file: `stubgen` did, and re-running
it silently replaced a working check with a placeholder that would not compile. Reach for that before believing any future "the real schema
works". Do not quote the compile result on its own again.

Eleven passes brought 33 924 down to zero, and **which pass was whose mistake is the part worth
keeping**:

| | errors | whose |
|---|---:|---|
| first run | 33 924 | -- |
| bind the eight custom scalars | 12 283 | **the harness**: `models:` maps a Go type, it does not register a `graphql.Scalar` |
| `Input[T]` honours `json:"-"` | 11 839 | the engine |
| `unbindableTypes` closes over containers | 11 395 | codegen |
| a method's parameter types are checked | -- | codegen; it removed the last *compile* error, not a build error |
| `ZeroForNullInputs` | 360 | a design decision, not a defect: see `ZeroForNull` in schema-build.md |
| `goIdent` exports a leading underscore | 18 | codegen |
| args structs pin the SDL name | 16 | codegen; only reachable once `goIdent` was fixed |
| an abstract type drops the pointer | 14 | codegen |
| a tag that names nothing in the schema falls back to the Go field name | 8 | the engine |
| a second Go type for one SDL enum is discovered | **0** | codegen |

**The last six were invisible to the compiler.** Every one produced Go that built cleanly
over 804 packages and a schema that could not be constructed. Four of them were found only
because a claim of "all that remains is the consumer's" was checked rather than trusted --
the first such claim was wrong by 342 errors, and the last by all eight that were left.

**Zero remain.** The schema builds with nothing hand-written but the ten `graphql.Scalar`
bindings its custom scalars need, which are the author's business by design -- a `models:`
entry chooses a Go type and says nothing about how it marshals.

The last eight were one kind: **two Go types for one SDL enum.** `@goModel` binds
`AssetDepreciationMethod` to `velox/asset.DepreciationMethod`, while the filter struct the
same ORM generates carries `velox/assetdepreciation.Method` for the same column;
`BudgetPeriodType` is the same. Both Go types exist, both carry the same values, and the
registry is keyed on `(name, reflect.Type)` -- so both bindings can live at once, and reading
this as the consumer's inconsistency was reading a supported case as a defect.
`altEnumTypes` now finds the second type through the bound input struct, `modelPkgs` loads
the package it lives in (no `AutoBind` pattern and no directive names it), and the binding is
emitted only when the type's constants cover **every** SDL value. A partial map would bind
some values and leave the rest unrepresentable at that position, which is a wrong answer at
run time where the unbound type is a refused build; what cannot be bound is reported instead,
naming the enum and the Go type so the author can write the map.

Only input objects are searched. An object field of the unbound type falls to a resolver,
which the author writes and can convert in; an input field has nowhere to put that conversion.

The six before them were the engine's, and reading them as the consumer's was the third time a
claim of "all that remains is theirs" was wrong. `AccessPolicyAssertion` carries
`RoleID int64 \`json:"role_id"\`` under an SDL that says `roleId`, which is an ORM naming its
own wire format rather than a mistake, and **the author could not fix it from outside**:
`Input` refuses a second binding for one type name. `autoInputFields` now falls back to the
Go field name for a tag that names nothing in the schema; see `schema-build.md`.

Two thirds of the first number was the harness, which is this file's recurring lesson under
another name: measure the thing, not your setup for measuring it.
