package codegen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// entgql binds an SDL enum to a struct holding a func, whose values are
// unexported package vars. graphql.Enum needs a map from Go value to SDL name,
// so neither half works: the type is not comparable and generated code cannot
// name a single value. One real schema does this 444 times, written into the
// SDL by the ORM, so there is nothing for the author to correct by hand.
func TestAnUnbindableEnumIsModelledInstead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT
enum OrderField @goModel(model: "hello/ent.OrderField") { WORKSPACE_ID CREATED_AT }
enum Status @goModel(model: "hello/ent.Status") { OPEN CLOSED }
type Doc { id: ID! orderBy: OrderField! status: Status! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Cursor struct{ ID string }

// OrderField is entgql's shape: not comparable, and its values are unexported.
type OrderField struct {
	Column   string
	ToCursor func(*Doc) Cursor
}

var orderByWorkspaceID = &OrderField{Column: "workspace_id"}

type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

type Doc struct {
	ID     string
	Status Status
}
`
	dir := writeEntModule(t, sdl, source)
	var notes []string
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		Notef:          func(f string, a ...any) { notes = append(notes, f) },
	})

	if strings.Contains(src, "graphql.Enum[ent.OrderField]") {
		t.Errorf("the unbindable enum was bound anyway\n%s", src)
	}
	// Status is bindable and must be untouched by the drop.
	if !strings.Contains(strings.Join(strings.Fields(src), " "), "ent.StatusOpen: \"OPEN\"") {
		t.Errorf("a bindable enum in the same package was dropped too\n%s", src)
	}
	if len(notes) != 1 {
		t.Errorf("the drop was reported %d times, want 1: %v", len(notes), notes)
	}

	// The generated model is what every field and argument then agrees on.
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(models), "type OrderField string") {
		t.Errorf("no model was generated for the dropped enum:\n%s", models)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// The real schema drops 444 enums and the report has to stay one readable
// line. Nothing in the suite gets near the truncation, so it is asked here.
func TestSummarizeTruncates(t *testing.T) {
	names := []string{"A", "B", "C", "D"}
	if got := summarize(names, 4); got != "A, B, C, D" {
		t.Errorf("summarize at the limit = %q", got)
	}
	if got := summarize(names, 2); got != "A, B and 2 more" {
		t.Errorf("summarize over the limit = %q", got)
	}
}

// Dropping an enum is not enough on its own. entgql puts the unbindable
// OrderField inside an Order input that is itself @goModel-bound to the ORM
// struct, so after the enum is dropped that struct still has a
// *ent.OrderField field and nothing registers the type -- 452 of the 11 839
// errors the real schema produced at NewSchema, every one of them an XOrder.
// The drop has to close over the types that hold what was dropped.
func TestADroppedEnumTakesItsContainerWithIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Direction { ASC DESC }
enum OrderField @goModel(model: "hello/ent.OrderField") { WORKSPACE_ID CREATED_AT }
input DocOrder @goModel(model: "hello/ent.DocOrder") {
  direction: Direction!
  field: OrderField!
}
type Doc { id: ID! }
type Query { docs(order: DocOrder): [Doc!]! }
`
	const source = `package ent

type Cursor struct{ ID string }

// OrderField is entgql's shape: not comparable, values unexported.
type OrderField struct {
	Column   string
	ToCursor func(*Doc) Cursor
}

var orderByWorkspaceID = &OrderField{Column: "workspace_id"}

type Direction string

const (
	DirectionAsc  Direction = "ASC"
	DirectionDesc Direction = "DESC"
)

type DocOrder struct {
	Direction Direction   ` + "`json:\"direction\"`" + `
	Field     *OrderField ` + "`json:\"field\"`" + `
}

type Doc struct{ ID string }
`
	dir := writeEntModule(t, sdl, source)
	var notes []string
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		Notef:          func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) },
	})

	if strings.Contains(src, "ent.DocOrder") {
		t.Errorf("DocOrder stayed bound to a struct holding a dropped type\n%s", src)
	}
	if strings.Contains(src, "ent.OrderField") {
		t.Errorf("OrderField stayed bound\n%s", src)
	}
	// Both are modelled instead, and the model is what every position agrees on.
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type OrderField string", "type DocOrder struct"} {
		if !strings.Contains(string(models), want) {
			t.Errorf("no model generated: %s\n%s", want, models)
		}
	}
	// Direction is bindable and untouched, so this is a reachability walk and
	// not "drop everything the ORM declares".
	if !strings.Contains(strings.Join(strings.Fields(src), " "), "ent.DirectionAsc: \"ASC\"") {
		t.Errorf("a bindable enum was dropped with the rest\n%s", src)
	}
	if len(notes) != 1 {
		t.Fatalf("want one report, got %d: %v", len(notes), notes)
	}
	if !strings.Contains(notes[0], "DocOrder") || !strings.Contains(notes[0], "OrderField") {
		t.Errorf("the report does not name both drops: %s", notes[0])
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}
