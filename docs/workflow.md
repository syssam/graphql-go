# The development loop

If you know gRPC, you already know the shape of this. Write the contract,
generate from it, implement the interface it gives you, wire it to a transport.

| gRPC | graphql-go |
|---|---|
| write `service.proto` | write `schema.graphql` |
| `protoc --go_out=. --go-grpc_out=.` | `go tool gqlc -config gqlc.yaml` |
| implement the generated `XxxServer` interface | implement the generated `Resolver` interface |
| `grpc.NewServer()`, `RegisterXxxServer` | `graph.NewSchema(r)`, `gqlhttp.New(exec)` |
| re-run `protoc` after editing the proto | re-run `go generate` after editing the SDL |

One difference worth knowing before you start: **`gqlc` never loads Go
packages.** It reads SDL and writes Go, and that is all. (`AutoBind`, below,
is the one thing that loads anything, it loads export data rather than a
module, and it is not reachable from the CLI at all.) A 200-entity schema
generates in under a second using 44 MB; gqlgen needs 31 s and 4.6 GB for the
same schema because it type-checks its own output. The cost of that choice is
that `gqlc` cannot guess at your Go types — you tell it, in `models:`, or you
let it infer from names and take a resolver where it cannot.

## 1. Write the schema

`schema.graphql` is the source of truth. Nothing generated is ever hand-edited.

```graphql
type User {
  id: ID!
  name: String!
  posts(first: Int = 10): [Post!]!
}

type Post {
  id: ID!
  title: String!
  author: User!
}

type Query {
  user(id: ID!): User
  posts: [Post!]!
}
```

## 2. Configure and generate

`gqlc.yaml` beside it:

```yaml
schema:
  - schema.graphql          # globs are allowed: schema/**/*.graphql
output: graph               # directory to write into
package: example.com/app/graph
nullableInputOmittable: true
models:
  Time: time.Time           # GraphQL scalar or type -> an existing Go type
```

Put the directive next to the code, not in a Makefile, so `go generate ./...`
finds it:

```go
//go:generate go tool gqlc -config gqlc.yaml
```

`go tool gqlc` rather than a `PATH` binary is deliberate: the version is pinned
in `go.mod`, so a generate step cannot silently resolve a different one than
the module records.

```sh
go generate ./...
```

Four things appear, all marked `DO NOT EDIT`:

| File | What it holds |
|---|---|
| `graph/model/models.go` | a Go struct per SDL type you did not map in `models:` |
| `graph/generated.go` | args structs, the `Resolver` interface, and the bindings |
| `graph/schema.go` | `NewSchema(r Resolver, opts ...graphql.SchemaOption)` |
| `graph/schema/*.graphql` | a copy of your SDL for the `//go:embed` |

Edit `schema.graphql` at the project root, never the copy under `graph/schema/`
— the next `go generate` overwrites it.

## 3. Implement the Resolver

This is the part that matters, and it is where the design differs from gqlgen
most visibly: **only the fields that need code are in the interface.** `User.id`
and `User.name` read a struct field, so nothing is generated for them and you
write nothing. `Post.author` is a lookup, so it is:

```go
type Resolver interface {
	User(ctx context.Context, args UserArgs) (*model.User, error)
	Posts(ctx context.Context) ([]*model.Post, error)
	PostAuthor(ctx context.Context, obj *model.Post) (*model.User, error)
	UserPosts(ctx context.Context, obj *model.User, args UserPostsArgs) ([]*model.Post, error)
}
```

Implement it on any type:

```go
type resolver struct{ db *sql.DB }

func (r *resolver) PostAuthor(ctx context.Context, obj *model.Post) (*model.User, error) {
	return r.users.Load(ctx, obj.AuthorID)   // a DataLoader; see below
}
```

A field is a resolver when `gqlc` cannot see how to read it off the struct. To
make one that *is* a struct field into a resolver anyway — or the reverse —
change the SDL or the `models:` mapping, not the generated file.

## 4. Wire it

```go
s, err := graph.NewSchema(&resolver{db: db})
if err != nil {
	return err                      // schema and bindings disagree; see below
}
exec := graphql.NewExecutor(s, graphql.WithMaxDepth(12) /* ... */)

mux.Handle("/graphql", gqlhttp.New(exec))
mux.Handle("/graphql/stream", gqlsse.New(exec, gqlsse.WithDrain(d)))
mux.Handle("/graphql/ws", gqlws.New(exec, gqlws.WithDrain(d)))
```

**Read [`operations.md`](operations.md) before this reaches anything
untrusted.** Four limits are off by default, and the drain has to be shut down
alongside `srv.Shutdown`, not before or after it.

## Where errors show up, and why that is the point

`NewSchema` returns every mismatch between your Go types and the SDL, joined,
at start-up:

```
graphql: type Comment has no Object binding
graphql: input Query.posts(first:): Go type int cannot represent null at level 0
of nullable type Int; use a pointer or slice there
```

That is the whole trade. The binding work happens once, when the process
starts, so the request path has no reflection and no type switching left to do.
It costs 30 ms and 11 MB at 200 entities — a long-lived server repays that in a
few hundred requests, a short-lived one does not.

## The DataLoader step nobody should skip

`Post.author` as written above is an N+1: a list of 50 posts is 50 lookups.
`loader` fixes it, and the executor is built to make it work — it announces a
wave of sibling fields before launching them, so every `Load` in that wave
queues before any of them dispatches:

```go
users := loader.New(func(ctx context.Context, ids []string) (map[string]*model.User, error) {
	return fetchUsers(ctx, ids)     // one query for the whole wave
})
```

Store it on the resolver, not per request — it keys its cache off the
`OperationContext`, so one instance is correct and one per request is waste.
[`examples/blog`](../examples/blog) does this for `Post.author`.

Alarm on `graphqlgo.loader.keys`: one key per span means batching has degraded
back to N+1, and that is the most useful alarm this library offers.

## Two ways to bind, and when codegen is the wrong one

Codegen is not required. Bindings are ordinary function values, so a small
service can skip the generate step entirely:

```go
graphql.Object[User]("User",
	graphql.Field("id", func(u *User) graphql.ID { return u.ID }),
	graphql.Field("name", func(u *User) string { return u.Name }),
)
```

[`examples/quickstart`](../examples/quickstart) is a whole server written that
way. Use codegen when the schema is large enough that writing those by hand is
bookkeeping — which is most schemas past a few dozen types — and hand-written
bindings when you want the wiring visible, as
[`examples/storefront`](../examples/storefront) does so its authorization is
readable in one file.

## Two richer binding modes, and where they are not

`gqlc.yaml` takes seven keys: `schema`, `output`, `package`,
`nullableInputOmittable`, `zeroForNullInputs`, `models` and the two directive
blocks (`modelDirective`, `fieldDirective`). The two modes below are **not** YAML
keys and cannot be reached from the CLI at all. They are fields on
`codegen.Config`, for a program that calls `codegen.Generate` itself -- which
is what an ORM or an in-house generator does, and the reason they exist.

- **`Config.Manifest`** binds types and fields outright instead of inferring
  them. It still loads no Go type information, so a method binding declares its
  own shape. A type in the manifest gets no inference at all: an unlisted field
  is a resolver.
- **`Config.AutoBind`** discovers the same bindings from named packages,
  loading only their export data rather than a whole module. Matching is json
  tag, then case-insensitive name, then a method whose shape the generator can
  call; anything else falls through to `Resolver`, because a resolver method can
  always be written, where a bad guess is a compile error in code you did not
  write.

## Migrating from gqlgen

Measured against a real 5 503-type ERP schema -- 828 SDL files, a Query root
4 154 fields wide, an ent-derived ORM underneath -- rather than reasoned about.
Every number below came from running the generator against it and compiling
what came out. The whole schema now generates in about 9 s and its 804
generated packages build and vet clean.

The work is one config file:

```yaml
schema:
  - schema/*.graphql
output: graph
package: example.com/app/graph

# 1. The bindings the SDL already carries. gqlgen writes them as
#    @goModel(model: "pkg/path.Type"); there were 3 778 of them.
modelDirective:
  name: goModel
  arg: model

# 2. The fields gqlgen was told to route through a resolver, which it spells
#    @goField(forceResolver: true). There were 4 567. This is not redundant
#    with the generator refusing a guess: it verifies a struct field exists,
#    where the author is saying the value must be computed. Binding it to the
#    column anyway compiles and answers the wrong thing.
fieldDirective:
  name: goField
  forceResolverArg: forceResolver

# 3. An ORM filter says `isNil: Boolean` in SDL and backs it with a plain
#    `bool`, reading false as "apply no predicate" -- so absent, null and
#    false are one value on purpose. The engine refuses that by default,
#    because in general a bool cannot tell absent from false. This says the
#    schema means it. 11 035 fields, and without it the schema does not build.
#    Read the graphql.ZeroForNull godoc first: for a PATCH-style input the
#    distinction is real and nullableInputOmittable is what you want instead.
zeroForNullInputs: true

# 4. The scalars, which gqlgen keeps in gqlgen.yml rather than in the SDL, so
#    they have to be moved by hand. There were about 40, and three of them
#    accounted for 1 400 fields.
models:
  ID: int64
  Time: time.Time
  Decimal: github.com/shopspring/decimal.Decimal
  Map: map[string]any
  JSON: map[string]any
```

```go
// 5. AutoBind, in code rather than YAML, pointed at the packages holding the
//    entities and the enum types.
cfg.AutoBind = []string{"example.com/app/ent", "example.com/app/schema/schematype"}
```

What each one is worth, counted as methods on the generated `Resolver`
interfaces:

| | resolver methods |
|---|---:|
| `modelDirective` only, no AutoBind | 10 716 |
| AutoBind on, nothing mapped | 18 581 |
| `ID` mapped to the ORM's integer id | 12 284 |
| `Time` mapped | 10 138 |
| `Decimal`, `Map`, `JSON` mapped | 8 724 |
| `fieldDirective` and `zeroForNullInputs` as well | 8 724 |

Four things that table says, in the order they matter:

**AutoBind makes the number go up before it goes down, and that is the point.**
Without it the generator infers: a scalar-returning field is read straight off
the struct. That inference is a guess, and where it is wrong -- `time.Time`
where the generated model says `model.Time` -- it is a compile error in a file
marked DO NOT EDIT. 10 716 against 18 581 is not 7 865 fields lost; it is
7 865 guesses stopped. Everything below buys them back with a verified binding
instead.

**`ID` first.** An ORM that stores ids as `int64` under an `ID` scalar fails
every id field against the default `graphql.ID`, and ids are the commonest
field in any schema: one line recovered 6 297.

**The last row moves nothing, and both settings are still required.** All 4 567
fields `fieldDirective` forces were computed fields the ORM type does not have,
so they were resolvers already; it earns its place on the field that *is* a
column and that the author wants computed anyway, where the generator would
otherwise bind the column, compile, and answer the wrong thing.
`zeroForNullInputs` does not touch resolvers at all -- it decides whether an
input *decodes*. Without it this schema does not build: `NewSchema` reports
11 035 errors and stops. A resolver count is the wrong instrument for it, which
is why it reads as a no-op here.

**8 724 is the floor for this configuration, not a backlog to configure
away.** What remains is dominated by fields the Go type genuinely does not
have -- `createdByUser`, `updatedByEmployee` and their kind -- which are
resolvers in any generator. The split was not counted, so take the shape and
not a percentage. Every AutoBind row above rose by exactly 398 when the
generator started checking a method's *parameter* types and not just their
count: those 398 are ORM edge methods taking their own `*ent.XOrder` against an
argument the generator models itself. They were bound before and did not
compile; they are resolvers now.

Three things worth knowing before the first run:

- **Name the packages holding your enum types too.** Their constants are found
  by the string each one carries, not by its identifier, and case does not have
  to match: ent stores a column lower-case and upper-cases it on the way out,
  so `EventTypeView = "view"` is the constant for the SDL value `VIEW`. Once
  `AutoBind` is set at all, a package the config already names in `models:`
  is loaded for its constants too, so you do not have to list five hundred
  of them twice. With `AutoBind` empty nothing is loaded, as before.
- **It does not reach into a nested struct.** ent keeps relations in `Edges`,
  and binding `Edges.ParentTest` directly would skip the lazy loader that
  `ParentTest(ctx)` runs -- returning null for data that exists. AutoBind
  already matches those as methods, which is correct.
- **An enum your ORM cannot express as constants is modelled rather than
  bound, and the generator says so.** entgql binds an SDL enum to a struct
  holding a func, whose values are unexported package vars: not comparable, and
  nothing outside that package can name a single value. There were 444 of them.
  The generated string enum is what every field, argument and resolver
  signature then agrees on, and the resolver translates it -- which it had to
  do regardless.

## Editing the schema afterwards

```sh
$EDITOR schema.graphql
go generate ./...
go build ./...        # a new resolver field is now a compile error; see below
```

The `Resolver` interface changing is the feature, but in one direction only.
**Adding** a field that needs a resolver grows the interface, so your type
stops satisfying it and `graph.NewSchema(&resolver{})` will not compile until
you write the method. **Removing** one shrinks the interface and compiles
fine -- Go does not mind a type having methods no interface names -- so a
deleted field leaves dead code that only a reading or a linter will find.
Keep the generated files in version control and in
CI — this repository's own CI re-runs `go generate` and fails if the tree
changes, which is what stops a schema edit from shipping without its bindings.
