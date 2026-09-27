# Large schemas without federation

One service, hundreds of entities, several teams: what that costs with
graphql-go, gqlc and velox, measured rather than estimated. Written
2026-09-26; every figure below is from a 4-core Linux container, go1.27.

## The project measured

300 velox entities in 30 domains of ten. Each has a name, a unique code, an
integer, a boolean, a nullable string, an enum and the time mixin; a
parent/children chain within its domain and a many-to-many link to the
previous domain; a Relay connection, a where input on four fields and two
edges, and create and update mutations. velox writes one SDL file per entity;
gqlc groups by file (one Go package and one `Resolver` per entity), binds
velox's entity types with `autoBind` and runs edge accessors inline.

| Step | Result |
|---|---|
| velox codegen | 19.5 s; 1.62M lines of Go in 4 516 files |
| gqlc | 6.5 s; 303 packages, 33 000 lines |
| Cold build | 4 min 31 s wall, 13 min 45 s CPU |
| `NewSchema` | 114 ms, 24 MB retained |
| Full introspection query | 4.3 MB, 27-67 ms |
| Binary | 198 MB (137 MB text); 215 MB before velox split its generator out |

Start-up and requests are not where a schema this size hurts. The edit loop is.

## The edit loop

| Change | Rebuild | Why |
|---|---|---|
| A resolver body | ~6.5 s | One package recompiles; the rest is linking a 198 MB binary |
| A hand-written SDL field in one group | one group package plus the link | gqlc regenerates only that group |
| A field on one entity | **3 min 3 s**, 589 s CPU, 608 packages | Below |

Adding a field to one entity changes velox's shared `entity`, `query` and
`filter` packages -- every entity's struct is in `entity`, because edges refer
from one to another -- and every package importing them recompiles: all 300
velox `client/<entity>` packages and all 300 gqlc group packages. Go's cache
is keyed by export data, so no layout of *per-entity* packages avoids this
while their types refer to each other. The CPU, by what recompiled:

| Packages | CPU before | CPU after |
|---|---|---|
| 300 gqlc group packages | 330 s | **214 s** |
| 300 velox `client/<entity>` | 181 s | 186 s |
| velox `query`, `filter`, `entity` | 169 s | 175 s |
| Total | 693 s (3 min 28 s wall) | 589 s (3 min 3 s wall) |

"Before" is the graphql-go this document was written against. 94% of each
group package's machine code was graphql-go's generic binding functions,
instantiated again in every package; moving their build-time logic into
non-generic functions halved it (212 KB to 109 KB for a velox-shaped group,
bounded by `TestBindingCodeSize`) with request-path benchmarks unchanged.
What remains of the group packages is the typed closures the executor calls
without reflection, which is the design.

The rebuild is CPU-bound, but only its middle is parallel. Timed from the
action graph (`go build -debug-actiongraph`) of a second entity edit:

| Phase | Wall | What runs |
|---|---|---|
| Head | 38 s | velox `filter`, then `entity`, which imports it: one package at a time |
| Middle | 110 s | the 300 group and 300 `client/<entity>` packages, 4 at a time |
| Tail | 39 s | velox `query`, then the `velox` root and `hook`, which import it |

`query` needs only `entity` and was ready at 38 s. It waited until 147 s
because Go's scheduler starts ready packages in the order a depth-first walk
from the build's roots reaches them. `gofmt` sorts `graph` ahead of `velox`
in any import list, so the walk reaches every group before it reaches
`query`, and `go build` has no flag to reorder it. A
repeat edit on a busier machine had the same shape: `query` ready at 67 s,
started at 282 s. More cores divide only
the middle. With the head and tail still serial, 16 cores would take this
rebuild from about 3 minutes to about 1 minute 45 seconds, not to 45 seconds.
That figure is derived from the 4-core timeline, not measured.

The head is velox's to shorten. `entity` imports `filter` only to name
`*filter.XWhereInput` in edge methods that take a `where` argument. Declared
against an interface instead, the two packages would compile side by side,
saving about 18 s. That was not done, for two reasons. gqlgen types each argument from the
method's parameter (`bindArgs`), so it would have to decode an input object
into an interface. And a nil `*XWhereInput` stored in an interface is not a
nil interface, which breaks the `where == nil` fast path the edge method
relies on.

## What to do about it

- **Give the developer machines and CI cores, up to a point.** An entity edit
  is fan-out across 600 packages, and that part divides by cores. About 77 s
  of it on this schema is two serial chains of velox's shared packages,
  which do not divide (above).
- **Import a group package, not the graph root, from domain code.** The root
  (`graph`) imports every group; a domain package that imports it recompiles
  whenever any group does.
- **Expect a resolver edit to cost the link**, about 6.5 s here. The binary
  was 215 MB and carried velox's code generator -- the compiler, jennifer,
  `golang.org/x/tools/go/packages`, gqlgen's codegen -- because the schema
  package imports velox's GraphQL annotations and velox's runtime imports the
  schema package, and the annotations and the generator were one package.
  velox has split them (`contrib/graphql` is annotations only, the generator is
  `contrib/graphql/graphqlgen`), and the same server is 198 MB and links
  1 214 packages instead of 1 280. `examples/veloxfx`'s server links none of
  the tooling.
- **Leave limits on.** `WithMaxDepth`, `WithMaxComplexity` and `QueryCost` do
  not charge introspection, so GraphiQL and client generators still work
  against a production configuration, and introspection is bounded by its own
  rule. A public API should still `DisableIntrospection()`.

## Not measured

- The same schema on ent and gqlgen. velox's own benchmark compares velox and
  Ent at 50 entities: an entity edit rebuilds in 17.0 s against 27.5 s
  (`velox/benchmarks/run.sh inc`).
- Memory of the cold build, and `gopls` on a workspace this size.
- Request latency under load at this schema size; `examples/veloxfx` has the
  per-request figures for a small schema.
