package benchmarks

import (
	"context"
	"embed"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/benchmarks/internal/data"
)

//go:embed schema.graphql
var schemaFS embed.FS

const (
	queryShallow = `{ users { id name email } }`
	queryNested  = `{ users { id friends { id name } } }`
)

func newGraphQLGo(users []*data.User) *graphql.Executor {
	s, err := graphql.NewSchema(graphql.SDLFS(schemaFS, "schema.graphql"),
		graphql.Object[data.User]("User",
			graphql.Field("id", func(u *data.User) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *data.User) string { return u.Name }),
			graphql.Field("email", func(u *data.User) string { return u.Email }),
			graphql.Field("friends", func(u *data.User) []*data.User { return u.Friends }),
		),
		graphql.Query(
			graphql.Resolve("users", func(_ context.Context, _ graphql.Root) ([]*data.User, error) {
				return users, nil
			}),
		),
	)
	if err != nil {
		panic(err)
	}
	return graphql.NewExecutor(s)
}
