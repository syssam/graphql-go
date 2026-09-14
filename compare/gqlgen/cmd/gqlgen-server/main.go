// Command gqlgen-server serves the same generated schema with gqlgen, for
// side-by-side comparison. See the note in cmd/graphqlgo-server on why the two
// run as separate processes.
//
//	go run ./cmd/gqlgen-server -addr :18081
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/99designs/gqlgen/graphql/handler"
	theirgen "github.com/syssam/graphql-go/compare/gqlgen/gen"
	"github.com/syssam/graphql-go/compare/gqlgen/gen/exec"
)

func main() {
	addr := flag.String("addr", ":18081", "listen address")
	flag.Parse()

	es := exec.NewExecutableSchema(exec.Config{Resolvers: &theirgen.Resolver{}})
	mux := http.NewServeMux()
	mux.Handle("/graphql", handler.NewDefaultServer(es))

	log.Printf("gqlgen listening on http://localhost%s/graphql", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
