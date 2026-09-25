package gqlfx_test

import (
	"strings"
	"testing"

	"go.uber.org/fx"

	categorygql "github.com/syssam/graphql-go/examples/veloxfx/graph/category"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
)

type notACategoryResolver struct{}

// A constructor for the wrong group is refused when the app is built, naming
// what it does not implement, not when a request first reaches the group.
func TestRegisterRefusesAResolverForAnotherGroup(t *testing.T) {
	app := fx.New(
		gqlfx.Register(func() *notACategoryResolver { return nil }, categorygql.Bindings),
		fx.Invoke(func(gqlfx.Groups) {}),
		fx.NopLogger,
	)
	err := app.Err()
	if err == nil || !strings.Contains(err.Error(), "does not implement") || !strings.Contains(err.Error(), "category.Resolver") {
		t.Fatalf("err = %v", err)
	}
}

// Each Register contributes exactly one binding to the group.
func TestRegisterContributesTheBindings(t *testing.T) {
	var got gqlfx.Groups
	app := fx.New(
		gqlfx.Register(func() categorygql.Resolver { return nil }, categorygql.Bindings),
		fx.Populate(&got),
		fx.NopLogger,
	)
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got.Bindings) != 1 || got.Bindings[0] == nil {
		t.Fatalf("bindings = %v", got.Bindings)
	}
}
