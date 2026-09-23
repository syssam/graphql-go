package fed_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/syssam/graphql-go/fed"
)

// A router sends every representation it needs in one _entities call, so a
// per-representation resolver is the canonical N+1. A DataLoader inside one
// does not help: Load blocks, and the representations are resolved in turn.
// BatchResolver receives every representation of its type at once.
func TestBatchResolverIsCalledOncePerType(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	e := newSubgraph(t,
		fed.BatchResolver("User", func(_ context.Context, reps []fed.Representation) ([]*user, error) {
			ids := make([]string, len(reps))
			out := make([]*user, len(reps))
			for i, r := range reps {
				ids[i], _ = r["id"].(string)
				out[i] = users[ids[i]]
			}
			mu.Lock()
			calls = append(calls, ids)
			mu.Unlock()
			return out, nil
		}),
		fed.Resolver("Product", resolveProduct),
	)
	resp := entities(t, e,
		`[{"__typename":"User","id":"1"},{"__typename":"Product","sku":"abc"},{"__typename":"User","id":"404"},{"__typename":"Nope"},{"__typename":"User","id":"1"}]`,
		`... on User { id name } ... on Product { sku }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"_entities":[{"id":"1","name":"Ada"},{"sku":"abc"},null,null,{"id":"1","name":"Ada"}]}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
	if len(calls) != 1 || strings.Join(calls[0], ",") != "1,404,1" {
		t.Fatalf("batch calls = %v, want one call with 1,404,1 in request order", calls)
	}
}

func TestBatchResolverMustAnswerEveryRepresentation(t *testing.T) {
	e := newSubgraph(t,
		fed.BatchResolver("User", func(context.Context, []fed.Representation) ([]*user, error) {
			return []*user{users["1"]}, nil
		}),
		fed.Resolver("Product", resolveProduct),
	)
	resp := entities(t, e, `[{"__typename":"User","id":"1"},{"__typename":"User","id":"2"}]`, `... on User { id }`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "returned 1 results for 2 representations") {
		t.Fatalf("errors = %v", resp.Errors)
	}
}

func TestBatchResolverErrorFailsTheField(t *testing.T) {
	e := newSubgraph(t,
		fed.BatchResolver("User", func(context.Context, []fed.Representation) ([]*user, error) {
			return nil, errors.New("datastore unavailable")
		}),
		fed.Resolver("Product", resolveProduct),
	)
	resp := entities(t, e, `[{"__typename":"User","id":"1"}]`, `... on User { id }`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "datastore unavailable") {
		t.Fatalf("errors = %v", resp.Errors)
	}
}
