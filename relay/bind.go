package relay

import (
	graphql "github.com/syssam/graphql-go"
)

// Pagination binds the parts of the Relay contract that every connection in
// a schema shares: the PageInfo object and the decoder for the four
// pagination arguments. Call it once per schema; Bind once per connection.
//
// It is separate from Bind because Bind is generic over the node type and
// these are not: folding them in would register PageInfo once per
// connection.
func Pagination() graphql.SchemaOption {
	return graphql.Options(
		graphql.Args[Args](),
		graphql.Object[PageInfo]("PageInfo",
			graphql.Field("hasNextPage", func(p *PageInfo) bool { return p.HasNextPage }),
			graphql.Field("hasPreviousPage", func(p *PageInfo) bool { return p.HasPreviousPage }),
			graphql.Field("startCursor", func(p *PageInfo) *string { return p.StartCursor }),
			graphql.Field("endCursor", func(p *PageInfo) *string { return p.EndCursor }),
		),
	)
}

// Bind binds Connection[T] and Edge[T] to the SDL types named. T is the node
// as the schema's other bindings model it, so for an object type bound with
// Object[user] that is *user.
//
// The SDL is still the contract: these types must already be declared.
func Bind[T any](connName, edgeName string) graphql.SchemaOption {
	return graphql.Options(
		graphql.Object[Edge[T]](edgeName,
			graphql.Field("node", func(e *Edge[T]) T { return e.Node }),
			graphql.Field("cursor", func(e *Edge[T]) string { return e.Cursor }),
		),
		graphql.Object[Connection[T]](connName,
			graphql.Field("edges", func(c *Connection[T]) []Edge[T] { return c.Edges }),
			graphql.Field("pageInfo", func(c *Connection[T]) *PageInfo { return &c.PageInfo }),
		),
	)
}
