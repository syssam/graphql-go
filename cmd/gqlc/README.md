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

Then implement `graph.Resolver` (one SDL group) or `graph.Resolvers` (two or
more groups) and call `graph.NewSchema(r, extra...)`. Custom scalars listed
under `models` must still be bound with `graphql.Scalar` in the `NewSchema`
options.

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
stays flat. Auto-bind and manifest mode are not generated yet.

The blog example is generated this way: `examples/blog/gqlc.yaml` writes
`examples/blog/graph`, and `examples/blog/internal/transport/graphql` implements `graph.Resolver`.
