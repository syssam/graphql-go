// Command compareserver serves the benchmark schema through either engine, so
// an external load generator can compare them.
//
//	go run ./cmd/compareserver -engine ours   -addr :18090
//	go run ./cmd/compareserver -engine gqlgen -addr :18095
//
// One engine per process, for the reason cmd/transportserver gives: two in one
// process share a garbage collector and a CPU budget, which is what a
// comparison is trying to isolate.
//
// **Both engines are fed by data.Dataset(), from one call, in one file.** That
// is the whole reason this is one command with a flag rather than two commands:
// cmd/transportserver builds its own dataset with different ids ("u0" against
// "1"), and a four-way comparison that pointed at it would have been measuring
// the fixture. The Node servers in the same suite reproduce data.Dataset()
// exactly and say so.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	gqlgenhandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/benchmarks/graph"
	"github.com/syssam/graphql-go/benchmarks/internal/data"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const sdl = `
type User { id: ID! name: String! email: String! friends: [User!]! }
type Query { users: [User!]! }
`

func ours(users []*data.User) http.Handler {
	s, err := graphql.NewSchema(graphql.SDL(sdl),
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
		log.Fatal(err)
	}
	// CSRF prevention off: the load generator sends no preflight header, and
	// the comparison is of execution, not of one server's default policy.
	return gqlhttp.New(graphql.NewExecutor(s), gqlhttp.WithCSRFPrevention(false))
}

func gqlgen(users []*data.User) http.Handler {
	es := graph.NewExecutableSchema(graph.Config{Resolvers: &graph.Resolver{Data: users}})
	h := gqlgenhandler.New(es)
	h.AddTransport(transport.POST{})
	// The same cache size as this engine's default plan cache. Both are
	// otherwise left at their defaults: the comparison is of two servers as an
	// author would reach for them, not of two tuning efforts.
	h.SetQueryCache(lru.New[*ast.QueryDocument](1024))
	return h
}

func main() {
	engine := flag.String("engine", "ours", "ours or gqlgen")
	addr := flag.String("addr", ":18090", "listen address")
	flag.Parse()

	users := data.Dataset()
	var h http.Handler
	switch *engine {
	case "ours":
		h = ours(users)
	case "gqlgen":
		h = gqlgen(users)
	default:
		log.Fatalf("unknown -engine %q; use ours or gqlgen", *engine)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", h)
	log.Printf("%s listening on %s with %d users", *engine, *addr, len(users))
	log.Fatal(http.ListenAndServe(*addr, mux))
}
