# Building a GraphQL service with velox, gqlc and fx

This guide walks through `examples/veloxfx` the way the gRPC basics tutorial
walks through a route guide: define the service, generate the code, implement
it, register it, run it, and then do the loop you will do every day -- add an
entity. Every output below is what the commands printed.

The service is a small shop: seven entities -- Category, Product, Customer,
Order, OrderItem, Warehouse, Stock -- each one package in each layer:

| Path | What | Written by |
|---|---|---|
| `graph/<entity>/generated.go` | bindings, argument structs, the `Resolver` interface | gqlc |
| `graph/<entity>/<entity>.resolvers.go` | the `Handler` implementing it, each method a thin hand-off | you, from gqlc's stubs |
| `graph/model/<entity>` | types the SDL declares and Go does not have | gqlc |
| `internal/service/<entity>` | the rules: an order's states, a stock count, who may change what | you |

An edit to one entity recompiles that entity's packages and not the rest.
Models sit apart from their entity because models of different entities
refer to each other, and a package each would import in a circle.

If you know gRPC, the mapping is close to one to one:

| gRPC | here |
|---|---|
| `.proto` messages and services | velox schema in `schema/`, plus `sdl/*.graphql` |
| `protoc` + `protoc-gen-go-grpc` | `go generate`: velox, then `go tool gqlc` |
| `XServer` interface | each entity's `Resolver` interface, `graph/<entity>/generated.go` |
| the handler implementing it | `graph/<entity>.Handler`, handing each call to `internal/service/<entity>` |
| `RegisterXServer(s, impl)` | `gqlfx.Register(<entity>.NewHandler, <entity>.Bindings)` in `resolvers.go` |
| `status.Errorf(codes.NotFound, ...)` | `apperr.New(apperr.NotFound, ...)`, `internal/apperr` |

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
  product: graph/product.Handler  # in the group's own package
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
| `graph/<entity>/generated.go` | the entity's `Resolver` interface, argument structs and `Bindings` | no |
| `graph/<entity>/<entity>.resolvers.go` | stubs for methods the `Handler` lacks, in the same package | yes -- yours from then on |
| `graph/model/<entity>/models.go` | types gqlc had to write, such as `PlaceOrderInput`; only order has any | no |
| `graph/schema.go` | `NewSchema(opts...)` over the embedded SDL | no |

A package gqlc stops writing is deleted on the next run, so a type that
moves to a velox binding does not leave its old model behind.

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

## 3. Implement: a service, and a resolver in front of it

**The rules are a service**, one package per entity in `internal/service`,
taking and returning velox's types, never GraphQL's argument structs:

```go
// internal/service/product/product.go
func (s *Service) Page(ctx context.Context, p PageArgs) (*entity.ProductConnection, error) {
	opts := []entity.ProductPaginateOption{entity.WithProductOrder(p.OrderBy)}
	if p.Where != nil {
		opts = append(opts, entity.WithProductFilter(p.Where.Filter))
	}
	return s.client.Product.Query().Paginate(ctx, p.After, p.First, p.Before, p.Last, opts...)
}

func (s *Service) Get(ctx context.Context, id int) (*entity.Product, error) {
	p, err := s.client.Product.Get(ctx, id)
	return p, velox.MaskNotFound(err) // an id naming nothing is null
}
```

**The entity's `Handler` hands each field to it**, beside the generated code
in the same package, and is the only place GraphQL's argument structs are
read:

```go
// graph/product/product.resolvers.go
func (r *Handler) Products(ctx context.Context, args ProductsArgs) (*entity.ProductConnection, error) {
	return r.svc.Page(ctx, productsvc.PageArgs(args))
}

func (r *Handler) Product(ctx context.Context, args ProductArgs) (*entity.Product, error) {
	return r.svc.Get(ctx, args.ID)
}
```

`PageArgs(args)` is a conversion, not a copy: the service declares the same
fields in the same order, and Go converts between struct types that differ
only in tags. Where the shapes differ -- `placeOrder`'s input -- the
`Handler` builds the service's type in four lines.

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
  `quantity = quantity - n WHERE quantity >= n` (`stock.Take`). Twenty
  concurrent orders for five units place exactly five; velox's
  `NonNegative()` does not check an `AddQuantity`. The database does
  (`gen.FeatureCheckBounds` puts a `CHECK (quantity >= 0)` on the column), so
  without the condition the orders past the fifth fail with
  `FAILED_PRECONDITION` rather than succeed -- the condition is still what
  turns them into a clean "not enough in stock".

**Errors carry a code**, the way gRPC errors carry a status, and every code
is one constant in `internal/apperr`, as gRPC's are in `codes`. A service's
rule raises its own:

```go
return apperr.New(apperr.FailedPrecondition, "order %d is %s, and this needs %v", id, o.Status, want)
```

and `errors.go` maps what velox raises onto the same codes and masks the
rest:

| `extensions.code` | when |
|---|---|
| `NOT_FOUND` | the id named no row |
| `BAD_USER_INPUT` | a field rule velox enforces, or an operation's own input check |
| `CONFLICT` | a unique column already holds the value |
| `FAILED_PRECONDITION` | a row still referenced, an order in the wrong status, not enough stock |
| `UNAUTHENTICATED` | nobody is signed in and this needs someone |
| `FORBIDDEN` | the viewer is signed in and may not do this |
| `INTERNAL_SERVER_ERROR` | anything else; the message is fixed and the real error is logged |
| `GRAPHQL_VALIDATION_FAILED` | the engine's own: the request itself is invalid |

## 4. Register and serve

`resolvers.go` registers every entity's `Handler` over its bindings, as a
gRPC server registers its services, and the services come from one more
module:

```go
// resolvers.go
var Resolvers = fx.Module("resolver",
	gqlfx.Register(category.NewHandler, category.Bindings),
	gqlfx.Register(product.NewHandler, product.Bindings),
	// ... one line per entity, and one gqlfx.Entity per federation type
)

// internal/service/service.go
var Module = fx.Module("service", fx.Provide(category.New, product.New /* ... */))
```

`Register` (`internal/gqlfx`, 10 lines) provides the Handler and adds the
entity's bindings to the `graphql` value group. It takes the Resolver
interface from `Bindings`, so a constructor for the wrong entity fails when
the app is built: `*product.Handler does not implement category.Resolver`.
`NewSchema` collects the group without naming a single entity, and
`veloxfx.go` is the three modules:

```go
var Module = fx.Module("veloxfx", service.Module, Resolvers, appCore)
```

fx runs start hooks in dependency order -- migrate, then listen -- and stop
hooks in reverse: drain streams, shut the server down, close the database.
An entity left out of `Resolvers` is a failed start that names the type it
left unbound, not a schema that fails the first request to reach it.

## 5. Who may read and write what

A public API needs four kinds of rule. Who may do what is the services'
decision, whoever calls them -- GraphQL, a job, another transport -- as
graphql.org recommends; what a query may *see* is GraphQL's, because only the
plan knows what was selected.

The policy itself is this shop's, not a recommendation: customers are listed
to anyone with their emails masked, orders are their owner's, stock levels are
staff's. Another application draws those lines elsewhere with the same four
mechanisms:

| Rule | Where | What it costs |
|---|---|---|
| Stock levels are internal | `@requiresScopes` on `Product.stocks`, put there by an annotation in `schema/catalog.go`, where the policy answers an empty list; and a read filter on every other stock query (`stock.HideLevels`): the `stocks` and `stock` roots, `Warehouse.stocks`, eager loads | nothing for `Product.stocks`: decided before anything resolves, so velox does not query stocks. Elsewhere one `WHERE false`: a shopper's list is empty and a lookup null, the same answer |
| An email is personal data | `@requiresScopes` on `Customer.email`; the policy masks it (`c*@example.com`), except a customer's own address, decided per row by `graphql.RedactRow` | the column is read, since masking rewrites a value |
| A customer sees only their orders | a velox read filter on every order query (`order.OwnOrders`), and on every order-line query, a line being theirs when its order is (`order.OwnOrderItems`) | one indexed `WHERE customer_orders = ?`, in the list, its count, a lookup by id and every eager load -- and for lines the same condition through their order, whether they are reached from an order, a product or the `orderItems` root; an anonymous read fails with `UNAUTHENTICATED` before any SQL |
| A customer moves only their orders | the same condition in every order `UPDATE` (`transition`, in `internal/service/order`), and `placeOrder` refusing a `customerID` not the viewer's with `FORBIDDEN` | nothing extra: a read interceptor never sees an `UPDATE`, so without it a customer could pay someone else's order and be told only `NOT_FOUND` |
| Every other write is staff's | each such service method calls `viewer.RequireStaff` -- `UNAUTHENTICATED` anonymously, `FORBIDDEN` for a customer -- and `mutationGate` in `authz.go` refuses the same calls again, first | the gate runs nothing: it reads the document, through fragments and without evaluating `@skip`, and denies any mutation not in `customerMutations`, so one added to the SDL is staff's until listed. It is not `@requiresScopes` because velox cannot put a directive on the mutations it generates |

And one kind of limit: `WithMaxDepth(10)` and a cost budget priced by the page
each connection asks for (`Connections: true`), both checked from the plan,
so a refused query runs no SQL. Without `Connections`, `first: 1` is priced
as fifty rows at every level, and legitimate pages get refused.

The viewer comes from an `X-Viewer` header -- `staff` or `customer:<id>` --
which stands in for verifying a token (`internal/viewer`); nothing past the
middleware reads anything but the `Viewer`.

### A federation subgraph

Other services in a federated graph refer to this one's products, customers
and orders. `key("id")` in `schema/*.go` puts `@key` on them, velox adds the
Federation v2 `@link`, and `gqlc.yaml`'s `federation: true` makes
`graph.NewSchema` take the entity resolvers. The service that owns each type
answers it -- `product.Service.Entity`, `customer.Service.Entity`,
`order.Service.Entity`, one `graphqlgo.Entities` each -- and `resolvers.go`
registers them with `gqlfx.Entity`. A router's batch of references is one query
per type, collected for what it selected, and an order fetched through the
router passes the same ownership filter as one fetched by a client
(`federation_scenarios_test.go`).

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

On PostgreSQL instead of the in-memory SQLite:

    go run ./cmd/server -driver postgres -dsn 'postgres://user:pass@localhost/veloxfx?sslmode=disable'

The pool keeps as many connections idle as it may open (`configurePool`,
20 by default, `Config.MaxConns`); `database/sql` keeps two, and on
PostgreSQL the reconnects that follow cost 9% of a replica's CPU and 37% of
a page's time under concurrent load (`BenchmarkOrderHistoryPageParallel`).
The whole suite runs on a server too, one database per test:

    VELOXFX_POSTGRES='postgres://postgres:pass@localhost:5432/postgres?sslmode=disable' go test -race ./...

The last one answers:

    {"data":{"orders":{"totalCount":1,"edges":[{"node":{"status":"PENDING","totalCents":10000,
      "customer":{"name":"Ada"},"items":[{"quantity":2,"product":{"sku":"kb-1"}}]}}]},
      "stocks":[{"quantity":3}]}}

## 7. The daily loop: add an entity

Adding a `Supplier`, with `Product.supplier` pointing at it -- done on a copy
of the example.

**Declare it.** The type in `schema/inventory.go` (22 lines), one edge on
Product, and which type implements the new group:

```go
edge.From("supplier", Supplier.Type).Ref("products").Unique(),
```

```yaml
scaffold:
  supplier:  graph/supplier.Handler
```

**Generate.** `go generate` (3.6 s, a second run over the same input) writes
`graph/supplier/generated.go` and, in the same package,
`graph/supplier/supplier.resolvers.go`:

```go
// Handler implements the supplier group's Resolver.
type Handler struct{}

var _ Resolver = (*Handler)(nil)

// CreateSupplier resolves Mutation.createSupplier.
func (r *Handler) CreateSupplier(ctx context.Context, args CreateSupplierArgs) (*entity.Supplier, error) {
	panic("not implemented: Mutation.createSupplier")
}

// UpdateSupplier resolves Mutation.updateSupplier.
// Suppliers resolves Query.suppliers.
```

Three stubs. `Supplier.id` binds to velox's `ID`, `Supplier.products` -- a
full connection -- to velox's edge method, and `Product.supplier` likewise;
none of them is yours to write.

**Build.** `go build ./...` passes, stubs and all.

**Test.** `go test` fails at start, before any request, saying what is left:

    failed to build *graphql.Schema: ... graphql: type Supplier has no Object binding

(fx prints its constructor chain above it; the line that matters is the last.)

**Implement.** The service, in `internal/service/supplier/supplier.go`, its
writes staff's like every other entity's:

```go
type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

func (s *Service) List(ctx context.Context) ([]*entity.Supplier, error) {
	q, err := s.client.Supplier.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

func (s *Service) Create(ctx context.Context, in supplierclient.CreateSupplierInput) (*entity.Supplier, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Supplier.Create().SetInput(in).Save(ctx)
}
// Update likewise.
```

and a service field, a constructor and one line per stub in the `Handler`:

```go
type Handler struct{ svc *suppliersvc.Service }

func (r *Handler) Suppliers(ctx context.Context) ([]*entity.Supplier, error) {
	return r.svc.List(ctx)
}
```

**Register.** One line in each module:

```go
supplier.New,                                          // internal/service/service.go
gqlfx.Register(supplier.NewHandler, supplier.Bindings), // resolvers.go
```

**Green, and it answers:**

    {"suppliers":[{"id":"1","name":"Keychron","products":{"totalCount":1,"edges":[{"node":{"sku":"kb-1"}}]}}]}

and a customer's `createSupplier` is refused with `FORBIDDEN` before it runs,
by the gate, with nothing added to it. Running `go generate` again leaves the
filled-in `Handler` byte for byte as it was; a field added later arrives as
one new stub at its end.

**64 lines by hand**, not counting blank ones: 23 of schema, 1 of
configuration, 4 of registration (two lines and their imports), 29 of service
(7 of them the staff checks), 6 in the `Handler`, and one in the test that
pins the group list. No signature typed, no type converted.

That is 25 more than the same entity took when each resolver held its own
queries (39). They are the service -- a second type, a constructor, a method
per operation -- and its staff checks. What they buy is that the rules do not
live in GraphQL code: a job or a test calls `supplier.Service` without an
argument struct and meets the same checks, and changing an entity's rules
recompiles its service and its own entity package, nothing else.

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
| `TestOrdersAreAFixedNumberOfQueries` | eager loading across entities, and no COUNT nobody asked for |
| `TestScenario*` (`scenarios_test.go`) | what large clients send: Relay fragments and variables over fifty orders, projected columns in the SQL, a count badge beside a list, a nested connection, a computed field under projection and in a nested page |
| `TestScenarioRouter*`, `TestScenarioServiceSDLIsComposable` (`federation_scenarios_test.go`) | a router's batch of thirteen references is two queries; several types, one query each; ownership holds through `_entities`; `_service` carries one federation link and every key |
| `TestScenario*` (`authz_scenarios_test.go`) | a withheld edge costs no query; masked personal data; a customer's orders filtered in SQL, by list, count, id and edge, and written only by their owner; catalog and stock writes staff-only; anonymous reads fail closed; deep and wide queries refused with no SQL, and an ordinary page is not |
| `TestEveryReadPathKeepsThePolicy` | every root, lookup and edge reaching a stock level, an order or an order line, as staff, the owner, another customer and nobody: 13 paths, 52 answers |
| `TestMutationGateRefusesEveryUnlistedMutation`, `TestMutationGateExpandsEachFragmentOnce` | every mutation the schema serves is refused to customers unless listed, through fragments and `@skip`; a fragment spread many times is walked once |
| `TestServicesRefuseNonStaffWrites`, `TestOrderServiceRefusesAnonymousWrites` | the services' own half: every staff-only write, and anonymous order writes, refused with no GraphQL in front and nothing changed |
| `TestUnclassifiedErrorsAreMasked` | a driver's error never reaches a client |
| `TestAMissingGroupFailsStart` | a missing registration fails start |
| `TestEveryEntityIsItsOwnGroup` | one package per entity, its `Handler` beside its generated code |
| `TestStopWaitsOutAnUnusedConnection` | shutdown survives a client's idle pre-connection |

## Reference: what velox cannot check for you

- **A validator checks the values velox writes, not the result of an
  `AddX`.** `AddQuantity(-n)` is `quantity = quantity + -n` in the database,
  so `NonNegative()` never sees the result. Generate with
  `gen.FeatureCheckBounds` so the database refuses it
  (`TestScenarioStockCannotGoNegativeWhateverThePath`), and take a count with
  a condition in the same statement, as `stock.Take` does (section 3).
