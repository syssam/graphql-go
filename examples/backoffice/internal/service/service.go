// Package service provides one service per entity, in
// internal/service/<entity>. A service is where an entity's rules are --
// the state an order moves through, that a stock count never goes below
// zero, which orders a viewer may touch -- and it takes and returns velox's
// types, never GraphQL's argument structs, so each entity's Handler in graph/
// is one caller of it and a job or another transport could be another.
//
// Two things it still shares with GraphQL. Its rules raise *graphql.Error
// with extensions.code set, which errors.go passes through. And its lists
// call velox's CollectFields, which loads what a GraphQL selection asks for;
// outside a GraphQL request it loads nothing extra and the list still works.
//
// Every rule about who may do what is a service's, whoever calls it: a write
// only staff may make calls viewer.RequireStaff, and the rows a viewer may
// read are narrowed in the query itself -- orders and their lines to the
// viewer's own, stock rows to those holding inventory:read -- so no path to
// them, root, edge or eager load, can go around the rule. GraphQL checks the
// same writes a second time, first: mutationGate in authz.go refuses them
// from the document before anything runs, and refuses a mutation added later
// until it is listed. How a field is shown -- a masked email, Product.stocks
// withheld without a query -- is GraphQL's, because only the plan knows what
// a query selects.
//
// The read rules, per entity, are pinned by TestEveryReadPathKeepsThePolicy.
package service

import (
	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/backoffice/internal/service/category"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/customer"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/order"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/orderitem"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/product"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/stock"
	"github.com/syssam/graphql-go/examples/backoffice/internal/service/warehouse"
	"github.com/syssam/graphql-go/fed"
)

// Services is every service, and all the GraphQL layer is given: gqlc's
// generated graph.Wire hands it to each entity's NewHandler, which keeps the
// service it needs. fx fills it in -- the services are built by fx, in
// whatever order they depend on one another -- and that is the last thing fx
// does for GraphQL: nothing in graph/ is registered, provided or looked up.
type Services struct {
	fx.In

	Category  *category.Service
	Product   *product.Service
	Customer  *customer.Service
	Order     *order.Service
	OrderItem *orderitem.Service
	Warehouse *warehouse.Service
	Stock     *stock.Service
}

// Entities are the federation entities, each answered by the service that
// owns the type.
func (s Services) Entities() []fed.Entity {
	return []fed.Entity{s.Product.Entity(), s.Customer.Entity(), s.Order.Entity()}
}

// Module provides every service.
var Module = fx.Module("service",
	fx.Provide(
		category.New,
		product.New,
		customer.New,
		order.New,
		orderitem.New,
		warehouse.New,
		stock.New,
	),
	// Row rules on reads, on whichever client the app ends up with -- a test
	// decorating it keeps them.
	fx.Invoke(order.OwnOrders, order.OwnOrderItems, stock.HideLevels),
)
