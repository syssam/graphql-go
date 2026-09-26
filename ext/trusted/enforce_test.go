package trusted_test

import (
	"context"
	"errors"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/ext/trusted"
)

const subDoc = `subscription { tick }`

// enforcedExecutor is what every transport shares, so refusing here is
// refusing on gqlhttp, gqlsse, gqlws, gqlecho and gqlfiber alike, whether or
// not each was given WithPersistedQueries.
func enforcedExecutor(t *testing.T) (*graphql.Executor, *int) {
	t.Helper()
	opened := new(int)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { hello: String! secret: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(
			graphql.Field("hello", func(graphql.Root) string { return "hi" }),
			graphql.Field("secret", func(graphql.Root) string { return "leaked" }),
		),
		graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) {
			*opened++
			ch := make(chan int)
			close(ch)
			return ch, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := trusted.NewStore(map[string]string{apq.Hash(doc): doc, "sub": subDoc})
	return graphql.NewExecutor(s, store.Enforce()...), opened
}

func TestEnforceRefusesUnlistedQueries(t *testing.T) {
	exec, _ := enforcedExecutor(t)

	ok := exec.Execute(context.Background(), &graphql.Request{Query: doc})
	defer ok.Release()
	if len(ok.Errors) != 0 {
		t.Fatalf("registered document refused: %v", ok.Errors)
	}

	refused := exec.Execute(context.Background(), &graphql.Request{Query: `{ secret }`})
	defer refused.Release()
	if got := codeOf(t, refused); got != apq.CodeNotInList {
		t.Fatalf("code = %q, want %q", got, apq.CodeNotInList)
	}
	if !refused.HasRequestErrors() {
		t.Fatal("refusal must be a request error, so transports give it the safelist's status")
	}
}

// Executor.Subscribe runs no request interceptor, so a request interceptor
// alone left every subscription outside the list -- and a refused one must
// not open its source.
func TestEnforceRefusesUnlistedSubscriptions(t *testing.T) {
	exec, opened := enforcedExecutor(t)

	_, err := exec.Subscribe(context.Background(), &graphql.Request{Query: `subscription { tick  }`})
	var se *graphql.SubscribeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *graphql.SubscribeError", err)
	}
	if got := codeOf(t, se.Response); got != apq.CodeNotInList {
		t.Fatalf("code = %q, want %q", got, apq.CodeNotInList)
	}
	if *opened != 0 {
		t.Fatal("refused subscription opened its source")
	}

	events, err := exec.Subscribe(context.Background(), &graphql.Request{Query: subDoc})
	if err != nil {
		t.Fatalf("registered subscription refused: %v", err)
	}
	for range events {
	}
	if *opened != 1 {
		t.Fatalf("source opened %d times, want 1", *opened)
	}
}
