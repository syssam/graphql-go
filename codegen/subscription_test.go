package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const subSDL = `
type Message { id: ID! body: String! }
type Query { ping: String! }
type Subscription { messages: Message! countdown(from: Int!): Int! }
`

func TestGenerateSubscriptionBindings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(subSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`graphql.Subscription(`,
		`graphql.Subscribe("messages", func(ctx context.Context) (<-chan *model.Message, error)`,
		`graphql.SubscribeArgs("countdown", func(ctx context.Context, a CountdownArgs) (<-chan int, error)`,
		`Messages(ctx context.Context) (<-chan *model.Message, error)`,
		`Countdown(ctx context.Context, args CountdownArgs) (<-chan int, error)`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("generated.go missing %s\n%s", want, src)
		}
	}
	// A subscription root field must not be emitted as a plain resolver: the
	// schema builder rejects that, so a regression here is a build failure
	// for every user with a subscription.
	for _, unwanted := range []string{`graphql.Resolve("messages"`, `graphql.ResolveArgs("countdown"`} {
		if strings.Contains(string(src), unwanted) {
			t.Errorf("generated.go still emits %s\n%s", unwanted, src)
		}
	}
}

func TestGeneratedSubscriptionExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(subSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)

	stub := `package graph

import (
	"context"
	"hello/graph/model"
)

type Stub struct{}

func (Stub) Ping(context.Context) (string, error) { return "pong", nil }

func (Stub) Messages(ctx context.Context) (<-chan *model.Message, error) {
	ch := make(chan *model.Message)
	go func() {
		defer close(ch)
		for _, m := range []*model.Message{{ID: "1", Body: "a"}, {ID: "2", Body: "b"}} {
			select {
			case ch <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (Stub) Countdown(ctx context.Context, a CountdownArgs) (<-chan int, error) {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for i := a.From; i > 0; i-- {
			select {
			case ch <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}

	testSrc := `package graph_test

import (
	"context"
	"hello/graph"
	"testing"

	"github.com/syssam/graphql-go"
)

func collect(t *testing.T, e *graphql.Executor, query string) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := e.Subscribe(ctx, &graphql.Request{Query: query})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var got []string
	for resp := range out {
		if len(resp.Errors) > 0 {
			t.Fatalf("event errors: %v", resp.Errors)
		}
		got = append(got, string(resp.Data))
		resp.Release()
	}
	return got
}

func TestSubscriptionEvents(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)

	got := collect(t, e, "subscription { messages { id body } }")
	want := []string{` + "`" + `{"messages":{"id":"1","body":"a"}}` + "`" + `, ` + "`" + `{"messages":{"id":"2","body":"b"}}` + "`" + `}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s", i, got[i], want[i])
		}
	}

	got = collect(t, e, "subscription { countdown(from: 2) }")
	want = []string{` + "`" + `{"countdown":2}` + "`" + `, ` + "`" + `{"countdown":1}` + "`" + `}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s", i, got[i], want[i])
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "exec_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}
