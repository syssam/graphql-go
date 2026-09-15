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
