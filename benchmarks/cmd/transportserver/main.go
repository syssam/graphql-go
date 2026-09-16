// Command transportserver serves the benchmark schema over one transport, so
// an external load generator can measure them against each other.
//
//	go run ./cmd/transportserver -transport fiber -addr :18090
//	k6 run -e URL=http://localhost:18090/graphql k6/transports.js
//
// One transport per process on purpose: two in one process share a garbage
// collector and a CPU budget, which is exactly what a transport comparison is
// trying to isolate.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

// Inline rather than embedded: go:embed cannot reach a file outside this
// directory, and duplicating the SDL here keeps the command buildable on its
// own.
const sdl = `
type User { id: ID! name: String! email: String! friends: [User!]! }
type Query { users: [User!]! }
`

// user mirrors the benchmark dataset without importing the internal package,
// which a command outside it cannot reach.
type user struct {
	ID      string
	Name    string
	Email   string
	Friends []*user
}

func dataset(n, friends int) []*user {
	users := make([]*user, n)
	for i := range users {
		users[i] = &user{
			ID:    fmt.Sprintf("u%d", i),
			Name:  fmt.Sprintf("User %d", i),
			Email: fmt.Sprintf("user%d@example.com", i),
		}
	}
	for i, u := range users {
		for j := 1; j <= friends; j++ {
			u.Friends = append(u.Friends, users[(i+j)%len(users)])
		}
	}
	return users
}

func newExecutor(users []*user) *graphql.Executor {
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
			graphql.Field("email", func(u *user) string { return u.Email }),
			graphql.Field("friends", func(u *user) []*user { return u.Friends }),
		),
		graphql.Query(
			graphql.Resolve("users", func(context.Context, graphql.Root) ([]*user, error) {
				return users, nil
			}),
		),
	)
	if err != nil {
		log.Fatalf("schema: %v", err)
	}
	return graphql.NewExecutor(s)
}

func main() {
	addr := flag.String("addr", ":18090", "listen address")
	name := flag.String("transport", "nethttp", "nethttp, echo, fiber or fiber-adaptor")
	users := flag.Int("users", 100, "dataset size")
	friends := flag.Int("friends", 3, "friends per user")
	flag.Parse()

	exec := newExecutor(dataset(*users, *friends))
	const path = "/graphql"

	log.Printf("%s listening on http://localhost%s%s", *name, *addr, path)
	switch *name {
	case "nethttp":
		mux := http.NewServeMux()
		mux.Handle(path, gqlhttp.New(exec))
		log.Fatal((&http.Server{Addr: *addr, Handler: mux}).ListenAndServe())
	case "echo":
		e := echo.New()
		e.Any(path, gqlecho.New(exec))
		log.Fatal(e.Start(*addr))
	case "fiber":
		app := fiber.New()
		app.All(path, gqlfiber.New(exec))
		log.Fatal(app.Listen(*addr))
	case "fiber-adaptor":
		// The row that exists to falsify the native path, not to flatter it.
		app := fiber.New()
		app.All(path, adaptor.HTTPHandler(gqlhttp.New(exec)))
		log.Fatal(app.Listen(*addr))
	default:
		fmt.Fprintf(os.Stderr, "unknown transport %q\n", *name)
		os.Exit(2)
	}
}
