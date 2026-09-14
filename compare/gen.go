//go:build ignore

// Command gen writes the comparison module for N entities.
//
//	go run gen.go -n 25
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/syssam/graphql-go/compare/internal/gen"
)

func main() {
	n := flag.Int("n", 25, "number of entities")
	flag.Parse()
	if err := gen.Generate(".", *n); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("generated %d entities\n", *n)
}
