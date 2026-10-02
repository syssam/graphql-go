package lint_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/syssam/graphql-go/lint"
)

func TestContextRace(t *testing.T) {
	// Point the analyzer at the fixture's own type so the test does not have
	// to depend on graphql-go.
	lint.ContextType = "a.OperationContext"
	analysistest.Run(t, analysistest.TestData(), lint.ContextRace, "a")
}

// The first test points the analyzer at a fixture type, so nothing checked the
// type name it ships with: renaming OperationContext would have left gqlvet
// matching nothing and reporting a clean tree. This one runs with the default,
// against a stand-in at the real import path.
func TestContextRaceWithTheShippedContextType(t *testing.T) {
	lint.ContextType = "github.com/syssam/graphql-go.OperationContext"
	analysistest.Run(t, analysistest.TestData(), lint.ContextRace, "b")
}
