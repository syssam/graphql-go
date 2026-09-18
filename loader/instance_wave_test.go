package loader_test

import (
	"context"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

type instanceCustomer struct {
	ID   string
	Name string
}

type objectAuthorizerFunc func(ctx context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error)

func (f objectAuthorizerFunc) AuthorizeObjects(ctx context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error) {
	return f(ctx, checks)
}

func allowEveryObject() graphql.ObjectAuthorizer {
	return objectAuthorizerFunc(func(_ context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error) {
		return make([]graphql.Outcome, len(checks)), nil
	})
}

func newGuardedLoaderExecutor(t *testing.T, ld *loader.Loader[string, string], a graphql.ObjectAuthorizer) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`
		directive @authorizeObject on OBJECT
		type Customer @authorizeObject { id: ID! name: String! owner: String! }
		type Query { customers: [Customer!]! }
	`),
		graphql.Object[instanceCustomer]("Customer",
			graphql.Field("id", func(c *instanceCustomer) graphql.ID { return graphql.ID(c.ID) }),
			graphql.Field("name", func(c *instanceCustomer) string { return c.Name }),
			graphql.Resolve("owner", func(ctx context.Context, c *instanceCustomer) (string, error) {
				return ld.Load(ctx, c.ID)
			}),
		),
		graphql.Query(graphql.Field("customers", func(graphql.Root) []instanceCustomer {
			return []instanceCustomer{
				{ID: "c1", Name: "one"},
				{ID: "c2", Name: "two"},
				{ID: "c3", Name: "three"},
			}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s, graphql.WithObjectAuthorizer(a))
}

func TestInstanceChecksShareTheWaveWithALoader(t *testing.T) {
	var batches, keys int
	// owner is resolved through a Loader for each of the three customers; one
	// batch proves the instance check did not split the wave.
	ld := loader.New(func(_ context.Context, ks []string) (map[string]string, error) {
		batches++
		keys += len(ks)
		out := make(map[string]string, len(ks))
		for _, k := range ks {
			out[k] = "owner-" + k
		}
		return out, nil
	})
	e := newGuardedLoaderExecutor(t, ld, allowEveryObject())
	resp := run(t, e, `{ customers { id owner } }`, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors = %v", resp.Errors)
	}
	if batches != 1 || keys != 3 {
		t.Fatalf("batches=%d keys=%d, want 1 batch of 3: the instance check fragmented the wave", batches, keys)
	}
}
