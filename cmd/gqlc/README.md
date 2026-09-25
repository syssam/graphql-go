# gqlc

`gqlc` generates `graphql` bindings from SDL. It does not load Go packages:
models are emitted from the schema, and fields that are not struct data
become methods on a `Resolver` interface.

```yaml
# gqlc.yaml
schema:
  - schema.graphql
output: graph
package: example.com/app/graph
nullableInputOmittable: true
models:
  Time: time.Time
```

```sh
go run github.com/syssam/graphql-go/cmd/gqlc -config gqlc.yaml
```

With one SDL group, implement `graph.Resolver` and call
`graph.NewSchema(r, extra...)`. With two or more, implement each group's
`Resolver` and pass its bindings, the way a gRPC server registers each service:

```go
graph.NewSchema(user.Bindings(userResolver), post.Bindings(postResolver), extra...)
```

There is no struct aggregating the groups. A group left out fails `NewSchema`
with the types it leaves unbound, where an unset field in an aggregate compiled,
built, and failed on the first request to reach it. Groups with no `Resolver`
are registered by `NewSchema` itself. Custom scalars listed under `models` must
still be bound with `graphql.Scalar` in the `NewSchema` options.

Each group is emitted as one `generated.go` holding its argument structs,
`Resolver` interface and bindings; the compiler rebuilds per package, so extra
files bought nothing.

Multiple SDL files (or `codegen.Config.GroupFunc`) split bindings *and*
models into per-group packages (`<group>/` and `model/<group>/`), so editing
one entity recompiles that group, the root package and `main` rather than the
whole schema. Models can be split because generated models hold only leaf
fields -- composite fields are resolver methods -- so object types never
reference each other. Input objects can, and a reference cycle between two
groups' inputs falls back to a single shared `model` package. A single group
stays flat.

`codegen.Config` also offers `AutoBind` (bind to existing Go types found by
package pattern) and `Manifest` (explicit bindings); `gqlc` does not expose
them yet, so call `codegen.Generate` from a small program to use them. An
unknown key in `gqlc.yaml` is an error rather than being ignored.

An interface or union whose members are all generated models becomes a Go
marker interface (`type Node interface{ IsNode() }`, with the method on each
member), so `Resolver` methods return `model.Node` rather than `any`. When a
member is bound to a type of your own, Go cannot add the method to it and the
abstract type stays `any`.

The blog example is generated this way: `examples/blog/gqlc.yaml` writes
`examples/blog/graph`, and `examples/blog/internal/transport/graphql`
implements `graph.Resolver`.
