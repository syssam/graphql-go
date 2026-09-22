package federation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
)

func execute(t *testing.T, e *graphql.Executor, query string, vars any) json.RawMessage {
	t.Helper()
	req := &graphql.Request{Query: query}
	if vars != nil {
		b, err := json.Marshal(vars)
		if err != nil {
			t.Fatalf("marshalling variables: %v", err)
		}
		req.Variables = b
	}
	resp := e.Execute(context.Background(), req)
	if len(resp.Errors) > 0 {
		t.Fatalf("query %s: %v", query, resp.Errors[0])
	}
	return json.RawMessage(resp.Data)
}

func subgraphs(t *testing.T) (products, reviews *graphql.Executor) {
	t.Helper()
	p, err := NewProducts()
	if err != nil {
		t.Fatalf("products subgraph: %v", err)
	}
	r, err := NewReviews()
	if err != nil {
		t.Fatalf("reviews subgraph: %v", err)
	}
	return p, r
}

// A router composes from what _service returns, so the first thing it does is
// ask. Both subgraphs must hand back the author's text as written -- the
// federation directives included, since those are what composition reads --
// and both must declare the same key for the entity they share, or the router
// has nothing to join on.
func TestBothSubgraphsPublishTheirSDL(t *testing.T) {
	products, reviews := subgraphs(t)

	for name, tc := range map[string]struct {
		exec *graphql.Executor
		want string
	}{
		"products": {products, ProductsSDL},
		"reviews":  {reviews, ReviewsSDL},
	} {
		t.Run(name, func(t *testing.T) {
			var got struct {
				Service struct{ SDL string } `json:"_service"`
			}
			if err := json.Unmarshal(execute(t, tc.exec, `{ _service { sdl } }`, nil), &got); err != nil {
				t.Fatalf("decoding _service: %v", err)
			}
			if got.Service.SDL != tc.want {
				t.Errorf("_service returned rewritten SDL:\n%s", got.Service.SDL)
			}
			if !strings.Contains(got.Service.SDL, `@key(fields: "sku")`) {
				t.Error("the shared entity's key is missing; the router has nothing to join on")
			}
		})
	}
}

// The whole point, end to end: `{ topProducts { name reviews { rating } } }`
// cannot be answered by either service. This is the fetch a router performs --
// query the owner, take the keys, ask the other subgraph about them by
// representation, merge by position -- written out so that every step is
// visible and every step is asserted.
func TestRouterJoinsAnEntityAcrossTwoSubgraphs(t *testing.T) {
	products, reviews := subgraphs(t)

	// Phase 1. The router adds __typename and the key fields to what the
	// client asked for; without __typename it cannot build a representation,
	// which is the mistake this test exists to catch.
	var phase1 struct {
		TopProducts []struct {
			Typename string `json:"__typename"`
			SKU      string `json:"sku"`
			Name     string `json:"name"`
		} `json:"topProducts"`
	}
	if err := json.Unmarshal(
		execute(t, products, `{ topProducts(first: 2) { __typename sku name } }`, nil),
		&phase1,
	); err != nil {
		t.Fatalf("decoding phase 1: %v", err)
	}
	if len(phase1.TopProducts) != 2 {
		t.Fatalf("phase 1 returned %d products, want 2", len(phase1.TopProducts))
	}

	// Phase 2. One representation per row, carrying the typename and the key
	// and nothing else.
	reps := make([]map[string]any, len(phase1.TopProducts))
	for i, p := range phase1.TopProducts {
		if p.Typename != "Product" {
			t.Fatalf("row %d has __typename %q, want Product", i, p.Typename)
		}
		reps[i] = map[string]any{"__typename": p.Typename, "sku": p.SKU}
	}

	var phase2 struct {
		Entities []struct {
			Reviews []struct {
				Rating int `json:"rating"`
			} `json:"reviews"`
			AverageRating *float64 `json:"averageRating"`
		} `json:"_entities"`
	}
	if err := json.Unmarshal(execute(t, reviews,
		`query($r: [_Any!]!) { _entities(representations: $r) { ... on Product { reviews { rating } averageRating } } }`,
		map[string]any{"r": reps},
	), &phase2); err != nil {
		t.Fatalf("decoding phase 2: %v", err)
	}

	// _entities answers positionally. A router merges on that and nothing
	// else, so a subgraph that reorders or drops is a silently wrong join
	// rather than an error.
	if len(phase2.Entities) != len(reps) {
		t.Fatalf("_entities returned %d rows for %d representations", len(phase2.Entities), len(reps))
	}

	// Merged. kb-01 has two reviews averaging 4, mn-27 has one of 4.
	if got := len(phase2.Entities[0].Reviews); got != 2 {
		t.Errorf("%s has %d reviews, want 2", phase1.TopProducts[0].SKU, got)
	}
	if phase2.Entities[0].AverageRating == nil || *phase2.Entities[0].AverageRating != 4 {
		t.Errorf("%s averageRating = %v, want 4", phase1.TopProducts[0].SKU, phase2.Entities[0].AverageRating)
	}
	if got := len(phase2.Entities[1].Reviews); got != 1 {
		t.Errorf("%s has %d reviews, want 1", phase1.TopProducts[1].SKU, got)
	}
	if phase1.TopProducts[0].Name != "Keyboard" {
		t.Errorf("phase 1 name = %q, want Keyboard: the join carried the wrong row", phase1.TopProducts[0].Name)
	}
}

// The join runs the other way too, and it is the direction that is easy to
// get wrong: the reviews subgraph owns Review and must hand the router a
// Product key it can take back to the owner, even though it holds nothing
// else about that product.
func TestRouterJoinsFromReviewsBackToProducts(t *testing.T) {
	products, reviews := subgraphs(t)

	// A client asking `{ latestReviews { body product { name } } }` would have
	// the router fetch the reviews, then resolve each product by key. This
	// subgraph exposes no product field on Review -- it has no sku to give
	// back that the router did not already have -- so the router's second hop
	// is driven from what it kept, which is the shape asserted here.
	var phase1 struct {
		LatestReviews []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"latestReviews"`
	}
	if err := json.Unmarshal(
		execute(t, reviews, `{ latestReviews(first: 3) { __typename id body } }`, nil),
		&phase1,
	); err != nil {
		t.Fatalf("decoding phase 1: %v", err)
	}
	if len(phase1.LatestReviews) != 3 {
		t.Fatalf("got %d reviews, want 3", len(phase1.LatestReviews))
	}

	// The router resolves the products it needs from the owner, by key,
	// exactly as it resolved reviews from the other side.
	reps := []map[string]any{
		{"__typename": "Product", "sku": "kb-01"},
		{"__typename": "Product", "sku": "mn-27"},
	}
	var phase2 struct {
		Entities []struct {
			Name  string `json:"name"`
			Price int    `json:"price"`
		} `json:"_entities"`
	}
	if err := json.Unmarshal(execute(t, products,
		`query($r: [_Any!]!) { _entities(representations: $r) { ... on Product { name price } } }`,
		map[string]any{"r": reps},
	), &phase2); err != nil {
		t.Fatalf("decoding phase 2: %v", err)
	}
	if len(phase2.Entities) != 2 {
		t.Fatalf("_entities returned %d rows, want 2", len(phase2.Entities))
	}
	if phase2.Entities[0].Name != "Keyboard" || phase2.Entities[1].Name != "Monitor" {
		t.Errorf("entities resolved to %q and %q, want Keyboard and Monitor",
			phase2.Entities[0].Name, phase2.Entities[1].Name)
	}
}

// A key one subgraph cannot resolve is a null in that position, not an error
// and not a shorter list. A router merges positionally, so a subgraph that
// dropped the row instead would shift every row after it onto the wrong
// product -- a wrong answer rather than a failed one, which is the worse of
// the two.
func TestUnresolvableKeyIsANullInPlace(t *testing.T) {
	products, _ := subgraphs(t)

	reps := []map[string]any{
		{"__typename": "Product", "sku": "kb-01"},
		{"__typename": "Product", "sku": "does-not-exist"},
		{"__typename": "Product", "sku": "mn-27"},
	}
	got := execute(t, products,
		`query($r: [_Any!]!) { _entities(representations: $r) { ... on Product { name } } }`,
		map[string]any{"r": reps})

	want := `{"_entities":[{"name":"Keyboard"},null,{"name":"Monitor"}]}`
	if string(got) != want {
		t.Fatalf("_entities =\n%s\nwant\n%s", got, want)
	}
}

// Both subgraphs declare Product, and the router's composition depends on
// them agreeing about the key. This pins that they do -- from the SDL each
// one publishes, not from a constant in this file, because that is what the
// router reads.
func TestBothSubgraphsAgreeOnTheSharedKey(t *testing.T) {
	products, reviews := subgraphs(t)

	keyOf := func(e *graphql.Executor) string {
		var got struct {
			Service struct{ SDL string } `json:"_service"`
		}
		if err := json.Unmarshal(execute(t, e, `{ _service { sdl } }`, nil), &got); err != nil {
			t.Fatalf("decoding _service: %v", err)
		}
		for line := range strings.SplitSeq(got.Service.SDL, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "type Product ") {
				return strings.TrimSpace(line)
			}
		}
		t.Fatal("no Product type in the published SDL")
		return ""
	}

	p, r := keyOf(products), keyOf(reviews)
	const key = `@key(fields: "sku")`
	if !strings.Contains(p, key) || !strings.Contains(r, key) {
		t.Fatalf("the two subgraphs disagree about Product's key:\n  products: %s\n  reviews:  %s", p, r)
	}
}
