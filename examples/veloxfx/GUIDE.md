# Building a GraphQL service with velox, gqlc and fx

This guide walks through `examples/veloxfx` the way the gRPC basics tutorial
walks through a route guide: define the service, generate the code, implement
it, register it, run it, and then do the loop you will do every day -- add an
entity. Every output below is what the commands printed.

The service is a small shop: seven entities in three domains.

| Domain | Entities | Package |
|---|---|---|
| catalog | Category, Product | `internal/catalog` |
| sales | Customer, Order, OrderItem | `internal/sales` |
| inventory | Warehouse, Stock | `internal/inventory` |

If you know gRPC, the mapping is close to one to one:

| gRPC | here |
|---|---|
| `.proto` messages and services | velox schema in `schema/`, plus `sdl/*.graphql` |
| `protoc` + `protoc-gen-go-grpc` | `go generate`: velox, then `go tool gqlc` |
| `XServer` interface | each group's `Resolver` interface, `graph/<group>` |
| `RegisterXServer(s, impl)` | `gqlfx.Register(NewImpl, <group>gql.Bindings)` in a domain's fx `Module` |
| `status.Error(codes.NotFound, ...)` | `extensions.code: "NOT_FOUND"`, from `errors.go` |

## Prerequisites

Go 1.27. Nothing else: the database is SQLite in memory, and velox, fx and
Echo come through `go.mod`. The example is its own module, so they never
become the library's dependencies.

    cd examples/veloxfx

## 1. Define the service

**The data model is velox schema**, one Go type per entity. This is the only
source of both the ORM and the GraphQL types:

```go
// schema/catalog.go
type Product struct{ velox.Schema }

func (Product) Fields() []velox.Field {
	return []velox.Field{
		field.String("sku").NotEmpty().Unique(),
		field.String("name").NotEmpty().Annotations(graphql.OrderField("NAME")),
		field.Int("price_cents").NonNegative().Annotations(graphql.OrderField("PRICE")),
	}
}

func (Product) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("category", Category.Type).Ref("products").Unique().Required(),
		edge.To("stocks", Stock.Type).Annotations(graphql.Skip(graphql.SkipInputs)),
	}
}

func (Product) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.RelayConnection(),                                 // products(first, after, where, orderBy)
		graphql.WhereInputFields("sku", "name", "price_cents"),    // filterable columns, opt-in
		graphql.WhereInputEdges("category"),
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}
```

Two decisions in there are about the API, not the data:

- **Which writes to expose.** velox would generate a create and an update for
  every entity, writing any column. Where a row carries a rule -- an order's
  status, an item's price, a stock count -- the entity gets no velox mutation,
  and its changes are operations written by hand (below).
- **`graphql.Skip(graphql.SkipInputs)` on to-many edges.** Without it the
  create and update inputs carry `stockIDs`, `addStockIDs`, ... and a client
  can move rows between parents by id.

A computed field is declared with the entity, together with what its
resolver reads, so velox loads it:

```go
// schema/sales.go, in Order's Annotations
graphql.Resolvers(
	graphql.Map("totalCents", "Int!").Loads("items"), // sums every item's price
),
```

**What velox does not generate is SDL in `sdl/`**, one file per entity:
lookups by id, deletes, domain operations.

```graphql
# sdl/order.graphql
extend type Mutation {
  placeOrder(input: PlaceOrderInput!): Order!
  payOrder(id: ID!): Order!
  shipOrder(id: ID!): Order!
  cancelOrder(id: ID!): Order!
  deleteOrder(id: ID!): ID!
}
```

**How the two become Go is `gqlc.yaml`.** The keys an ORM-backed service
needs:

```yaml
models:
  Time: time.Time
  ID: int                         # velox keys rows by int
modelDirective: { name: goModel, arg: model }
autoBind: [./velox/entity]        # bind SDL types to velox's own entity types
groups:
  trimPrefix: velox_              # velox_product.graphql is the product group
  rename: { schema: root }        # velox's shared file is root
rootFields: returnType            # createProduct goes to the product group
zeroForNullInputs: true           # velox's clearX: Boolean is a Go bool
inlineAccessors: true             # edge accessors read loaded edges: no goroutine each
inline: [Order.totalCents]        # nor this resolver, which sums loaded items
scaffold:                         # which type implements each group
  product: internal/catalog.ProductResolver
  # ...
```

`cmd/gqlc/README.md` says what each key does.

## 2. Generate the code

    go generate

It runs velox (`generate.go`), then `go tool gqlc -config gqlc.yaml`, in about
six seconds. What comes out:

| Path | What | Edit it? |
|---|---|---|
| `velox/` | the ORM: clients, queries, entities, migrations | no |
| `graph/<group>/generated.go` | the group's `Resolver` interface, argument structs and `Bindings` | no |
| `graph/schema.go` | `NewSchema(opts...)` over the embedded SDL | no |
| `internal/<domain>/<group>.resolvers.go` | stubs for methods the implementation lacks | yes -- yours from then on |

A group's interface holds only what nothing else answers. For product:

```go
type Resolver interface {
	CreateProduct(ctx context.Context, args CreateProductArgs) (*entity.Product, error)
	UpdateProduct(ctx context.Context, args UpdateProductArgs) (*entity.Product, error)
	DeleteProduct(ctx context.Context, args DeleteProductArgs) (int, error)
	Products(ctx context.Context, args ProductsArgs) (*entity.ProductConnection, error)
	Product(ctx context.Context, args ProductArgs) (*entity.Product, error)
}
```

Everything else is bound for you: columns to struct fields, `id` to velox's
`ID int`, and edges -- including `Category.products`, a paged, filtered,
ordered connection -- to velox's generated edge methods, whose arguments
(`*entity.ProductOrder`, `*filter.ProductWhereInput`, `*gqlrelay.Cursor`) are
velox's own types.

## 3. Implement the resolvers

A resolver is the business logic and nothing else. The arguments are already
velox's types, so there is nothing to convert:

```go
// internal/catalog/product.resolvers.go
func (r *ProductResolver) Products(ctx context.Context, args productgql.ProductsArgs) (*entity.ProductConnection, error) {
	opts := []entity.ProductPaginateOption{entity.WithProductOrder(args.OrderBy)}
	if args.Where != nil {
		opts = append(opts, entity.WithProductFilter(args.Where.Filter))
	}
	q := r.client.Product.Query() // Paginate loads what the page selects
	return q.Paginate(ctx, args.After, args.First, args.Before, args.Last, opts...)
}

func (r *ProductResolver) Product(ctx context.Context, args productgql.ProductArgs) (*entity.Product, error) {
	p, err := r.client.Product.Get(ctx, args.ID)
	return p, velox.MaskNotFound(err) // an id naming nothing is null
}
```

Three habits, each because of something velox does:

- **Let the query decide what loads.** `Paginate` reads the page's
  selection, and a list resolver calls `CollectFields(ctx)` before `All`:
  each selected edge is one query for every row, only selected columns are
  read, and there is no `COUNT(*)` unless `totalCount` is asked for.
  `orders { edges { node { customer items { product } } } }` over three
  orders of two items is four queries; one edge at a time it would be
  thirteen. velox reads the selection through its `contrib/graphqlgo`
  module, which `server.go` installs with one `graphqlgo.Collect()`.
- **Declare what a computed field reads.** velox cannot see inside
  `Order.totalCents`, which sums every item's price; `.Loads("items")` on its
  `graphql.Map` tells it, and wherever orders are collected the items are
  loaded whole, once for all of them. Undeclared, a client selecting
  `items { quantity }` projects the price away and the total is 0, and a
  customer list with order totals takes a query per order.
- **Take a count with a condition**, never a read-then-write:
  `quantity = quantity - n WHERE quantity >= n` (`inventory.Take`). Twenty
  concurrent orders for five units place exactly five; velox's
  `NonNegative()` does not check an `AddQuantity`, so without the condition
  all twenty succeed and the count ends at -15.

**Errors carry a code**, the way gRPC errors carry a status. A domain rule
raises its own:

```go
return (&graphql.Error{Message: fmt.Sprintf("order %d is %s, and this needs %v", id, o.Status, want)}).
	WithExtension("code", "FAILED_PRECONDITION")
```

and `errors.go` maps what velox raises and masks the rest:

| `extensions.code` | when |
|---|---|
| `NOT_FOUND` | the id named no row |
| `BAD_USER_INPUT` | a field rule velox enforces, or an operation's own input check |
| `CONFLICT` | a unique column already holds the value |
| `FAILED_PRECONDITION` | a row still referenced, an order in the wrong status, not enough stock |
| `INTERNAL_SERVER_ERROR` | anything else; the message is fixed and the real error is logged |
| `GRAPHQL_VALIDATION_FAILED` | the engine's own: the request itself is invalid |

## 4. Register and serve

Each domain registers its groups in one `Module`, as a gRPC server registers
its services:

```go
// internal/catalog/module.go
var Module = fx.Module("catalog",
	gqlfx.Register(NewCategoryResolver, categorygql.Bindings),
	gqlfx.Register(NewProductResolver, productgql.Bindings),
)
```

`Register` (`internal/gqlfx`, 10 lines) provides the Resolver and adds the
group's bindings to the `graphql` value group. It takes the Resolver interface
from `Bindings`, so a constructor for the wrong group fails when the app is
built: `*catalog.ProductResolver does not implement category.Resolver`.

`NewSchema` collects the `graphql` group without naming a single entity, and
`veloxfx.go` lists the domains:

```go
var Domains = fx.Options(catalog.Module, sales.Module, inventory.Module)
```

fx runs start hooks in dependency order -- migrate, then listen -- and stop
hooks in reverse: drain streams, shut the server down, close the database.
A domain left out is a failed start that names the type it left unbound,
not a schema that fails the first request to reach it.

## 5. Who may read what

A public API needs three kinds of rule, and each has one right place
(`authz.go`):

| Rule | Where | What it costs |
|---|---|---|
| Stock levels are internal | `@requiresScopes` on `Product.stocks`, put there by an annotation in `schema/catalog.go`; the policy answers an empty list | nothing: the decision is made before anything resolves, and velox does not query stocks for a viewer who may not see them |
| An email is personal data | `@requiresScopes` on `Customer.email`; the policy masks it (`c*@example.com`) | the column is read, since masking rewrites a value |
| A customer sees only their orders | a velox read filter on every order query (`ownOrders`) | one indexed `WHERE customer_orders = ?`, in the list, its count, a lookup by id and every eager load; an anonymous read fails with `UNAUTHENTICATED` before any SQL |

And one kind of limit: `WithMaxDepth(10)` and a cost budget priced by the page
each connection asks for (`Connections: true`), both checked from the plan,
so a refused query runs no SQL. Without `Connections`, `first: 1` is priced
as fifty rows at every level, and legitimate pages get refused.

The viewer comes from an `X-Viewer` header -- `staff` or `customer:<id>` --
which stands in for verifying a token (`internal/viewer`); nothing past the
middleware reads anything but the `Viewer`.

## 6. Call it

    go run ./cmd/server

    GQL='curl -s localhost:8080/graphql -H content-type:application/json -H X-Viewer:staff'
    $GQL -d '{"query":"mutation { createCategory(input:{name:\"Keyboards\"}) { id } }"}'
    $GQL -d '{"query":"mutation { createProduct(input:{sku:\"kb-1\",name:\"Board\",priceCents:5000,categoryID:\"1\"}) { sku category { name } } }"}'
    $GQL -d '{"query":"mutation { createCustomer(input:{name:\"Ada\",email:\"ada@example.com\"}) { id } }"}'
    $GQL -d '{"query":"mutation { createWarehouse(input:{name:\"North\"}) { id } }"}'
    $GQL -d '{"query":"mutation { createStock(input:{quantity:5,warehouseID:\"1\",productID:\"1\"}) { id } }"}'
    $GQL -d '{"query":"mutation { placeOrder(input:{customerID:\"1\",warehouseID:\"1\",items:[{productID:\"1\",quantity:2}]}) { totalCents } }"}'
    $GQL -d '{"query":"{ orders(first:10) { totalCount edges { node { status totalCents customer { name } items { quantity product { sku } } } } } stocks { quantity } }"}'

The last one answers:

    {"data":{"orders":{"totalCount":1,"edges":[{"node":{"status":"PENDING","totalCents":10000,
      "customer":{"name":"Ada"},"items":[{"quantity":2,"product":{"sku":"kb-1"}}]}}]},
      "stocks":[{"quantity":3}]}}

## 7. The daily loop: add an entity

Adding a `Supplier` to inventory, with `Product.supplier` pointing at it --
done on a copy of the example.

**Declare it.** The type in `schema/inventory.go` (21 lines), one edge on
Product, and which type implements the new group:

```go
edge.From("supplier", Supplier.Type).Ref("products").Unique(),
```

```yaml
scaffold:
  supplier:  internal/inventory.SupplierResolver
```

**Generate.** `go generate` (5.4 s) writes `graph/supplier` and
`internal/inventory/supplier.resolvers.go`:

```go
// SupplierResolver implements the supplier group's Resolver.
type SupplierResolver struct{}

var _ suppliergql.Resolver = (*SupplierResolver)(nil)

// CreateSupplier resolves Mutation.createSupplier.
func (r *SupplierResolver) CreateSupplier(ctx context.Context, args suppliergql.CreateSupplierArgs) (*entity.Supplier, error) {
	panic("not implemented: Mutation.createSupplier")
}

// UpdateSupplier resolves Mutation.updateSupplier.
// Suppliers resolves Query.suppliers.
// ...
```

Three stubs. `Supplier.id` binds to velox's `ID`, `Supplier.products` -- a
full connection -- to velox's edge method, and `Product.supplier` likewise;
none of them is yours to write.

**Build.** `go build ./...` passes, stubs and all.

**Test.** `go test` fails at start, before any request, saying what is left:

    failed to build *graphql.Schema: ... graphql: type Supplier has no Object binding

(fx prints its constructor chain above it; the line that matters is the last.)

**Implement and register.** A client field and a constructor on the type, a
body for each stub --

```go
func (r *SupplierResolver) Suppliers(ctx context.Context) ([]*entity.Supplier, error) {
	q, err := r.client.Supplier.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}
```

-- and one line in `internal/inventory/module.go`:

```go
gqlfx.Register(NewSupplierResolver, suppliergql.Bindings),
```

**Green, and it answers:**

    {"data":{"suppliers":[{"id":"1","name":"Keychron","products":{"totalCount":1,"edges":[{"node":{"sku":"kb-1"}}]}}]}}

Running `go generate` again leaves the filled-in file byte for byte as it was;
a field added later arrives as one new stub at its end.

**39 lines by hand:** 22 of schema, 3 of configuration and registration, 13
of bodies and constructor, and one in the test that pins the group list.
(35 when `Suppliers` named its eager loads, `WithProducts().All(ctx)`; the
four more lines are what makes it load only what a query selects.) No
signature typed, no type converted. (The first version of this example took
55 for the same entity, every signature copied from the interface and every
id and order argument converted by hand.)

## 8. Test it

    go test -race ./...

The suite drives the real app over HTTP, and each rule it pins was checked by
removing the rule and watching the test fail:

| Test | Pins |
|---|---|
| `TestTheAPIOffersOperationsNotRawWrites` | no raw write on a rule-carrying entity; no input attaches rows by id |
| `TestProductsPageFilterAndOrder` | cursor paging, filters across an edge, ordering, a nested connection |
| `TestPlaceOrderIsOneTransaction` | all-or-nothing order placement; price taken at order time |
| `TestOrderLifecycle` | the state machine, and stock returned on cancel |
| `TestConcurrentOrdersCannotOversell` | twenty racing orders for five units place five |
| `TestReadAndDeleteByID` | null for a missing id; what a delete may take with it |
| `TestOrdersAreAFixedNumberOfQueries` | eager loading across domains, and no COUNT nobody asked for |
| `TestScenario*` (`scenarios_test.go`) | what large clients send: Relay fragments and variables over fifty orders, projected columns in the SQL, a count badge beside a list, a nested connection, a computed field under projection and in a nested page |
| `TestScenario*` (`authz_scenarios_test.go`) | a withheld edge costs no query; masked personal data; a customer's orders filtered in SQL, by list, count, id and edge; anonymous reads fail closed; deep and wide queries refused with no SQL, and an ordinary page is not |
| `TestUnclassifiedErrorsAreMasked` | a driver's error never reaches a client |
| `TestAMissingDomainFailsStart` | a missing registration fails start |
| `TestStopWaitsOutAnUnusedConnection` | shutdown survives a client's idle pre-connection |

## Reference: what velox cannot check for you

- **A validator checks the values velox writes, not the result of an
  `AddX`.** `AddQuantity(-n)` is `quantity = quantity + -n` in the database,
  so `NonNegative()` never sees the result. Take a count with a condition in
  the same statement, as `inventory.Take` does (section 3).
