// Package orderby converts each connection's orderBy argument from the type
// gqlc modelled to velox's. It is its own leaf package because any domain
// with an edge to a sortable entity needs the conversion, and keeping it with
// the owning domain made two domains import each other: sales takes stock
// through inventory, and inventory pages a warehouse's orders.
//
// A nil order is velox's default, by id.
package orderby

import (
	ordermodel "github.com/syssam/graphql-go/examples/veloxfx/graph/model/order"
	productmodel "github.com/syssam/graphql-go/examples/veloxfx/graph/model/product"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

func Product(o *productmodel.ProductOrder) (*entity.ProductOrder, error) {
	if o == nil {
		return nil, nil
	}
	f, err := resolve.OrderField[entity.ProductOrderField](string(o.Field))
	if err != nil {
		return nil, err
	}
	return &entity.ProductOrder{Direction: o.Direction, Field: f}, nil
}

func Order(o *ordermodel.OrderOrder) (*entity.OrderOrder, error) {
	if o == nil {
		return nil, nil
	}
	f, err := resolve.OrderField[entity.OrderOrderField](string(o.Field))
	if err != nil {
		return nil, err
	}
	return &entity.OrderOrder{Direction: o.Direction, Field: f}, nil
}
