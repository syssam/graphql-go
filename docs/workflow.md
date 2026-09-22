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
packages.** It reads SDL and writes Go, and that is all. A 200-entity schema
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

`gqlc.yaml` takes exactly five keys: `schema`, `output`, `package`,
`nullableInputOmittable` and `models`. The two modes below are **not** YAML
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
