// Serves a gqlhttp handler for the GraphQL-over-HTTP audit suite; see
// README.md. It lives under testdata so the go tool never builds it.
package main

import (
	"context"
	"log"
	"net/http"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

type varArgs struct{ V *string }

func main() {
	s, err := graphql.NewSchema(
		graphql.SDL(`
type Query {
  hello: String!
  echo(v: String): String
}
type Mutation { bump: String! }`),
		graphql.Args[varArgs](graphql.InputField("v", func(a *varArgs, v *string) { a.V = v })),
		graphql.Query(
			graphql.Field("hello", func(graphql.Root) string { return "world" }),
			graphql.ResolveArgs("echo", func(_ context.Context, _ graphql.Root, a varArgs) (*string, error) {
				return a.V, nil
			}),
		),
		graphql.Mutation(graphql.Field("bump", func(graphql.Root) string { return "bumped" })),
	)
	if err != nil {
		log.Fatal(err)
	}
	h := gqlhttp.New(graphql.NewExecutor(s), gqlhttp.WithCSRFPrevention(false))
	log.Println("READY http://127.0.0.1:4151/")
	log.Fatal(http.ListenAndServe("127.0.0.1:4151", h))
}
