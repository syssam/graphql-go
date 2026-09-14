// Command graphqlgo-server serves the generated schema with graphql-go.
//
//	go run gen.go -n 200
//	go run ./cmd/graphqlgo-server -addr :18080
//
// It is a separate binary from the gqlgen server on purpose: running both in
// one process makes them share a garbage collector and a CPU budget, which
// skews any load measurement taken against either.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/syssam/graphql-go/compare"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

func main() {
	addr := flag.String("addr", ":18080", "listen address")
	flag.Parse()

	e, err := compare.NewGraphQLGo()
	if err != nil {
		log.Fatalf("schema: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(e))

	log.Printf("graphql-go listening on http://localhost%s/graphql", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
