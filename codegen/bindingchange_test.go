package codegen

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const bindingChangeSDL = `
type Task {
  id: ID!
  createdAt: String!
  title: String!
  slug: String!
}
type Query { task(id: ID!): Task }
`

func taskManifest(fields map[string]FieldBinding) *Manifest {
	return &Manifest{Types: []TypeBinding{{
		Name:   "Task",
		Go:     GoType{PkgPath: "example.com/app/ent", Name: "Task"},
		Fields: fields,
	}}}
}

// A field that moves between a Resolver method and the bound type changes
// where its value comes from, and the regenerated file shows that only as a
// rewritten generated.go. An ORM gaining a CreatedAt field rebound
// Task.createdAt from a resolver that derived it to the stored column, and
// the consumer found out by reading the diff.
func TestABindingChangeIsNamedByGenerateAndCheck(t *testing.T) {
	t.Parallel()
	dir := generate(t, bindingChangeSDL, Config{Manifest: taskManifest(map[string]FieldBinding{
		"id":    {Kind: FieldStruct},
		"title": {Kind: FieldStruct},
		"slug":  {Kind: FieldMethod, GoName: "Slug"},
	})})
	after := taskManifest(map[string]FieldBinding{
		"id":        {Kind: FieldStruct},
		"createdAt": {Kind: FieldStruct, GoName: "CreatedAt"},
		"title":     {Kind: FieldMethod, GoName: "DisplayTitle"},
		"slug":      {Kind: FieldMethod, GoName: "Slug"},
	})
	want := []string{
		"Task.createdAt: resolver TaskCreatedAt → field CreatedAt",
		"Task.title: field Title → method DisplayTitle",
	}
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		Manifest: after,
	}

	err := Check(context.Background(), cfg, CheckOptions{})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("check: err = %v, want ErrStale", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("check does not say %q:\n%v", w, err)
		}
	}
	if strings.Contains(err.Error(), "Task.slug") || strings.Contains(err.Error(), "Task.id") {
		t.Errorf("check names a field whose binding did not change:\n%v", err)
	}

	var notes []string
	cfg.Notef = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(notes, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("generate does not say %q; notes:\n%s", w, all)
		}
	}

	// Unchanged now: nothing to say.
	notes = nil
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "Task.") }) {
		t.Errorf("a generate with nothing changed names a binding: %q", notes)
	}
}

// What answers each field is read from the generated source, so every shape
// the emitters write has to be recognised; one that is not reads as no
// binding, and a change to or from it goes unreported.
func TestBindingSourcesReadsEveryEmittedShape(t *testing.T) {
	t.Parallel()
	src := []byte(`package graph

func Bindings(r Resolver) graphql.SchemaOption {
	return graphql.Options(
		graphql.Object[ent.Task]("Task",
			graphql.Field("id", func(v *ent.Task) graphql.ID { return graphql.ID(v.ID) }),
			graphql.Field("slug", func(v *ent.Task) string { return v.Slug() }),
			graphql.FieldArgs("price", func(v *ent.Task, a TaskPriceArgs) float64 { return v.Price(a.Rate) }),
			graphql.Resolve("stock", func(ctx context.Context, v *ent.Task) (int, error) { return v.Stock(ctx) }, graphql.Inline()),
			graphql.Resolve("owner", func(ctx context.Context, v *ent.Task) (*ent.User, error) { return r.TaskOwner(ctx, v) }),
			graphql.ResolveArgs("tags", func(ctx context.Context, v *ent.Task, a TaskTagsArgs) ([]string, error) { return r.TaskTags(ctx, v, a) }),
		),
		graphql.Query(
			graphql.ResolveArgs("task", func(ctx context.Context, _ graphql.Root, a QueryTaskArgs) (*ent.Task, error) { return r.QueryTask(ctx, a) }),
		),
		graphql.Subscription(
			graphql.Subscribe("ticks", func(ctx context.Context) (<-chan int, error) { return r.SubscriptionTicks(ctx) }),
		),
		graphql.Args[QueryTaskArgs](),
	)
}
`)
	got := bindingSources(src, map[string]string{"Query": "Query", "Subscription": "Subscription"})
	want := map[string]string{
		"Task.id":            "field ID",
		"Task.slug":          "method Slug",
		"Task.price":         "method Price",
		"Task.stock":         "method Stock",
		"Task.owner":         "resolver TaskOwner",
		"Task.tags":          "resolver TaskTags",
		"Query.task":         "resolver QueryTask",
		"Subscription.ticks": "resolver SubscriptionTicks",
	}
	if len(got) != len(want) {
		t.Errorf("got %d bindings, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
