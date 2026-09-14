// Command gqlvet runs graphql-go's own static checks.
//
//	go run github.com/syssam/graphql-go/lint/cmd/gqlvet ./...
//
// It is a vet tool, so it also works as:
//
//	go vet -vettool=$(which gqlvet) ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/syssam/graphql-go/lint"
)

func main() { singlechecker.Main(lint.ContextRace) }
