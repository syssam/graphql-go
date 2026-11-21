package codegen

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// generateFiles runs Generate over several SDL files, so that each is a group.
func generateFiles(t *testing.T, files map[string]string, cfg Config) (string, error) {
	t.Helper()
	dir := t.TempDir()
	sdl := map[string]string{}
	for name, src := range files {
		sdl["schema/"+name] = src
	}
	writeFiles(t, dir, sdl)
	cfg.Dir = dir
	cfg.SchemaGlobs = []string{"schema/*.graphql"}
	cfg.Output = "graph"
	cfg.Package = "hello/graph"
	return dir, Generate(context.Background(), cfg)
}

// A root type is bound to graphql.Root and no model is generated for it, so
// a field returning one -- the relay habit of a mutation payload handing back
// `query: Query!` -- has to be typed graphql.Root as well. It was
// *model.Query, which is undefined. The query is executed through the field
// as well as validated, since the claim is that the executor treats the value
// as the root object, not that the output compiles.
func TestARootTypeAsAFieldTypeIsTheEngineRoot(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := generate(t, `
type Payload { ok: Boolean query: Query! }
type Query { a: Int }
type Mutation { doIt: Payload }
`, Config{})
	src := generated(t, dir, "generated.go")
	if !strings.Contains(src, "(*graphql.Root, error)") {
		t.Errorf("Payload.query is not typed *graphql.Root:\n%s", src)
	}
	writeTempModule(t, dir)
	writeFiles(t, dir, map[string]string{
		"graph/stub.go": `package graph

import (
	"context"

	"github.com/syssam/graphql-go"
	"hello/graph/model"
)

type Stub struct{}

func (Stub) A(context.Context) (*int, error) { n := 7; return &n, nil }

func (Stub) DoIt(context.Context) (*model.Payload, error) { ok := true; return &model.Payload{Ok: &ok}, nil }

func (Stub) PayloadQuery(context.Context, *model.Payload) (*graphql.Root, error) { return &graphql.Root{}, nil }
`,
		"graph/exec_test.go": `package graph_test

import (
	"context"
	"testing"

	"github.com/syssam/graphql-go"
	"hello/graph"
)

func TestRootThroughAField(t *testing.T) {
	t.Parallel()
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: "mutation { doIt { ok query { a } } }"})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors[0])
	}
	if want := ` + "`" + `{"doIt":{"ok":true,"query":{"a":7}}}` + "`" + `; string(resp.Data) != want {
		t.Fatalf("data = %s, want %s", resp.Data, want)
	}
}
`,
	})
	validateGenerated(t, dir, "hello/graph")
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

// With StructObjectFields a model holds its object-, interface- and
// union-valued fields, and an abstract type with a marker is a generated type
// in its group's model package. A.n: Bnode is therefore an import of model/b
// from model/agrp, and B.a: A the import back; the cycle check skipped every
// abstract reference and split the models into a cycle Go refuses.
func TestAMarkerReferenceCountsTowardsTheModelImportCycle(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, err := generateFiles(t, map[string]string{
		"a.graphql": "type A { n: Bnode }\ntype Query { a: A }\n",
		"b.graphql": "interface Bnode { id: ID! }\ntype B implements Bnode { id: ID! a: A }\n",
	}, Config{StructObjectFields: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generatedSources(t, dir), "IsBnode()") {
		t.Fatal("Bnode got no marker, so this does not exercise the marker edge")
	}
	validateGenerated(t, dir, "hello/graph")
}

// The positive control for the edge: a marker referenced one way only must not
// cost the per-group split, or "always share one package" would pass the test
// above.
func TestAOneWayMarkerReferenceKeepsTheSplit(t *testing.T) {
	t.Parallel()
	dir, err := generateFiles(t, map[string]string{
		"a.graphql": "type A { n: Bnode }\ntype Query { a: A }\n",
		"b.graphql": "interface Bnode { id: ID! }\ntype B implements Bnode { id: ID! }\n",
	}, Config{StructObjectFields: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generated(t, dir, "model/agrp/models.go"), "N b.Bnode") {
		t.Errorf("models were not split per group:\n%s", generated(t, dir, "model/agrp/models.go"))
	}
}

// A root is graphql.Root, in no model package, so a reference to one is no
// edge: A.q: Query beside B.a: A is not a cycle, and the models stay split.
func TestARootReferenceAddsNoModelEdge(t *testing.T) {
	t.Parallel()
	dir, err := generateFiles(t, map[string]string{
		"a.graphql": "type A { id: ID! q: Query }\n",
		"b.graphql": "type Query { a: A }\ntype B { a: A }\n",
	}, Config{StructObjectFields: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(norm(generated(t, dir, "model/agrp/models.go")), "Q *graphql.Root") {
		t.Errorf("models were not split per group:\n%s", generatedSources(t, dir))
	}
}

// Enum constants are Type+Value, so two enums can produce one constant: A._B
// and AX.B are both AXB, A._ID and AX.ID both AXID. Nothing compared constants
// across enums, and the output did not compile.
func TestEnumConstantsAcrossEnumsAreRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ sdl, a, b, ident string }{
		{"enum A { _B }\nenum AX { B }\n", "A._B", "AX.B", "AXB"},
		{"enum A { _ID }\nenum AX { ID }\n", "A._ID", "AX.ID", "AXID"},
	} {
		_, err := generateWith(t, c.sdl+"type Query { a: A x: AX }\n", Config{})
		if err == nil {
			t.Errorf("%q: generated, want a refusal", c.sdl)
			continue
		}
		for _, want := range []string{c.a, c.b, `"` + c.ident + `"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error does not name %s: %v", c.sdl, want, err)
			}
		}
	}
}

// A constant collides only within one model package: the same pair in two
// groups, split per group, is two packages and compiles.
func TestEnumConstantsInDifferentModelPackagesAreAllowed(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, err := generateFiles(t, map[string]string{
		"one.graphql": "enum A { _B }\ntype Query { a: A }\n",
		"two.graphql": "enum AX { B }\nextend type Query { x: AX }\n",
	}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	validateGenerated(t, dir, "hello/graph")
}

// The collision check walked the Resolver methods, which leave out a field the
// model answers, while every field with arguments gets an args struct. A
// manifest method on FeatureFlag.targetingRules(x:) beside the resolver
// FeatureFlagTargeting.rules(x:) wrote FeatureFlagTargetingRulesArgs twice.
func TestArgsStructCollisionCountsModelAnsweredFields(t *testing.T) {
	t.Parallel()
	_, err := generateWith(t, `
type FeatureFlag { id: ID! targetingRules(x: Int): String }
type FeatureFlagTargeting { id: ID! rules(x: Int): String }
type Query { flag: FeatureFlag t: FeatureFlagTargeting }
`, Config{Manifest: &Manifest{Types: []TypeBinding{{
		Name: "FeatureFlag",
		Go:   GoType{PkgPath: "hello/ent", Name: "FeatureFlag"},
		Fields: map[string]FieldBinding{
			"id":             {Kind: FieldStruct},
			"targetingRules": {Kind: FieldMethod, Context: true, Error: true},
		},
	}}}})
	if err == nil {
		t.Fatal("generated two FeatureFlagTargetingRulesArgs, want a refusal")
	}
	for _, want := range []string{"args struct", "FeatureFlag.targetingRules", "FeatureFlagTargeting.rules", `"FeatureFlagTargetingRulesArgs"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// A group whose only resolve-kind fields are manifest methods with a context
// or an error has nothing for a Resolver to do: the model answers them. It
// got `type Resolver interface {}` and Bindings(r Resolver), which NewSchema
// did not register and Wire and Scaffold demanded a target for. It is a group
// with no Resolver now, registered by NewSchema itself.
func TestAGroupWhoseFieldsTheModelAnswersHasNoResolver(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, err := generateFiles(t, map[string]string{
		"query.graphql": "type Query { thing: Thing }\n",
		"thing.graphql": "type Thing { id: ID! stock: Int }\n",
	}, Config{Manifest: &Manifest{Types: []TypeBinding{{
		Name: "Thing",
		Go:   GoType{PkgPath: "hello/ent", Name: "Thing"},
		Fields: map[string]FieldBinding{
			"id":    {Kind: FieldStruct},
			"stock": {Kind: FieldMethod, Context: true, Error: true},
		},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	thing := generated(t, dir, "thing/generated.go")
	if strings.Contains(thing, "type Resolver interface") || !strings.Contains(thing, "func Bindings() graphql.SchemaOption") {
		t.Errorf("thing/generated.go still asks for a Resolver:\n%s", thing)
	}
	if !strings.Contains(generated(t, dir, "schema.go"), "thing.Bindings(),") {
		t.Errorf("NewSchema does not register the thing group itself:\n%s", generated(t, dir, "schema.go"))
	}
	// The positive control: the root group still has its Resolver.
	if !strings.Contains(generated(t, dir, "query/generated.go"), "type Resolver interface") {
		t.Errorf("the query group lost its Resolver")
	}
	writeTempModule(t, dir)
	writeFiles(t, dir, map[string]string{"ent/ent.go": `package ent

import (
	"context"

	"github.com/syssam/graphql-go"
)

type Thing struct{ ID graphql.ID }

func (*Thing) Stock(context.Context) (*int, error) { return nil, nil }
`})
	validateGenerated(t, dir, "hello/graph")
}

// An interface or union without a marker is any and declares no Go type, so
// an enum constant of the same name collides with nothing: Kind.NODE is
// KindNode, and union KindNode over a mapped member has no marker.
func TestEnumConstantBesideAnUnemittedAbstractTypeIsAllowed(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, err := generateWith(t, `
enum Kind { NODE }
type M { id: ID! }
union KindNode = M
type Query { k: Kind n: KindNode }
`, Config{Models: map[string]string{"M": "hello/ent.M"}})
	if err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	writeFiles(t, dir, map[string]string{"ent/ent.go": "package ent\n\nimport \"github.com/syssam/graphql-go\"\n\ntype M struct{ ID graphql.ID }\n"})
	validateGenerated(t, dir, "hello/graph")

	// The positive control: with an unmapped member the union has a marker,
	// is declared, and the same constant is refused.
	_, err = generateWith(t, `
enum Kind { NODE }
type M { id: ID! }
union KindNode = M
type Query { k: Kind n: KindNode }
`, Config{})
	if err == nil || !strings.Contains(err.Error(), "Kind.NODE") {
		t.Errorf("a constant beside a declared marker was not refused: %v", err)
	}
}
