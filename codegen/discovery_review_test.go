package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// writeAltEnumModuleAt is the altEnumSDL fixture with the filter's second type
// in pkgDir under package pkgName, which need not be the directory's name.
func writeAltEnumModuleAt(t *testing.T, pkgDir, pkgName string) string {
	t.Helper()
	entSrc := strings.Replace(altEnumEnt, `import "hello/method"`, `import `+pkgName+` "hello/`+pkgDir+`"`, 1)
	entSrc = strings.Replace(entSrc, "*method.Method", "*"+pkgName+".Method", 1)
	dir := writeEntModule(t, altEnumSDL, entSrc)
	src := strings.Replace(altEnumPkg, "package method", "package "+pkgName, 1)
	writeFiles(t, dir, map[string]string{pkgDir + "/enum.go": src})
	return dir
}

// The second Go type's package was imported unaliased under its directory's
// last element, which is not its name when the two differ (velox puts mutation
// inputs in .../client/todo under package todoclient) and is not a package at
// all for a major-version element.
func TestASecondEnumTypeIsImportedUnderTheQualifierItIsReferredTo(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package load")
	}
	// qualifier is what pathQualifier gives the path: the rule mapped packages
	// follow, so a second enum type's package reads as a mapped one would.
	for _, tc := range []struct{ dir, name, qualifier string }{
		{"client/method", "methodclient", "method"},
		{"method/v2", "method", "method"},
		{"method.v1", "method", "methodv1"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			dir := writeAltEnumModuleAt(t, tc.dir, tc.name)
			src := autoGenerate(t, dir, Config{ModelDirective: ModelDirective{Name: "goModel", Arg: "model"}})
			if !strings.Contains(src, tc.qualifier+` "hello/`+tc.dir+`"`) {
				t.Fatalf("the second type's package is not imported as %s\n%s", tc.qualifier, src)
			}
			buildGenerated(t, dir)
		})
	}
}

// Two input structs carrying two different Go types for one SDL enum: each is
// a binding the registry can hold, and dropping the second left every input
// field of that type a NewSchema error, with nothing said.
func TestEverySecondGoTypeForAnEnumIsBound(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Method @goModel(model: "hello/ent.DepreciationMethod") { STRAIGHT_LINE NONE }
input MethodFilter { method: Method }
input OtherFilter { method: Method }
input PartialFilter { method: Method }
type Asset { id: ID! method: Method! }
type Query { assets(filter: MethodFilter!, other: OtherFilter, partial: PartialFilter): [Asset!]! }
`
	const entSrc = `package ent

import (
	"hello/method"
	"hello/othermethod"
	"hello/partialmethod"
)

type DepreciationMethod string

const (
	DepreciationMethodStraightLine DepreciationMethod = "straight_line"
	DepreciationMethodNone         DepreciationMethod = "none"
)

type MethodFilter struct{ Method *method.Method }
type OtherFilter struct{ Method *othermethod.Method }
type PartialFilter struct{ Method *partialmethod.Method }

type Asset struct {
	ID     string
	Method DepreciationMethod
}
`
	dir := writeQualModule(t, sdl, entSrc, map[string]string{
		"method":        altEnumPkg,
		"othermethod":   strings.Replace(altEnumPkg, "package method", "package othermethod", 1),
		"partialmethod": "package partialmethod\n\ntype Method string\n\nconst MethodNone Method = \"none\"\n",
	})
	var notes []string
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		Notef:          func(f string, a ...any) { notes = append(notes, f) },
	})
	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{
		`graphql.Enum[method.Method]("Method"`,
		`graphql.Enum[othermethod.Method]("Method"`,
		`othermethod.MethodNone: "NONE"`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated code does not contain %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "partialmethod") {
		t.Errorf("bound a second type whose STRAIGHT_LINE has no constant\n%s", src)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "second Go type") }) {
		t.Errorf("the partial third type was not reported: %v", notes)
	}
	buildGenerated(t, dir)
}

// Written values are a statement and discovered ones a guess, so the statement
// wins value by value, as it does field by field. They were dropped whenever
// discovery also produced the type, and the derived ent.StatusArchived does not
// exist.
func TestExplicitEnumValuesWinOverDiscovery(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
enum Status @goModel(model: "hello/ent.Status") { ACTIVE ARCHIVED }
type Item { id: ID! status: Status! }
type Query { items: [Item!]! }
`
	const entSrc = `package ent

type Status string

const (
	StatusActive Status = "active"
	StatusGone   Status = "gone"
)

type Item struct {
	ID     string
	Status Status
}
`
	dir := writeEntModule(t, sdl, entSrc)
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		Manifest: &Manifest{Types: []TypeBinding{{
			Name:   "Status",
			Values: map[string]string{"ARCHIVED": "StatusGone", "ACTIVE": "StatusActive"},
		}}},
	})
	if flat := strings.Join(strings.Fields(src), " "); !strings.Contains(flat, `ent.StatusGone: "ARCHIVED"`) {
		t.Errorf("the explicit ARCHIVED constant was dropped\n%s", src)
	}
	buildGenerated(t, dir)
}

// fillNonZero sets every settable field reachable from v to a non-zero value,
// so a struct field added later is set without this test being edited.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Struct:
		for _, field := range v.Fields() {
			fillNonZero(t, field)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, k)
		fillNonZero(t, e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(t, s.Index(0))
		v.Set(s)
	default:
		t.Fatalf("fillNonZero: %s has no non-zero value here; decide how mergeManifests treats it", v.Type())
	}
}

// Every TypeBinding field has a merge decision: an explicit binding that sets
// it must come out of the merge with it set. Values was the one left out, and
// a field added later is caught here rather than in a consumer's generate.
func TestMergeManifestsKeepsEveryExplicitTypeBindingField(t *testing.T) {
	t.Parallel()
	var explicit TypeBinding
	fillNonZero(t, reflect.ValueOf(&explicit).Elem())
	discovered := TypeBinding{Name: explicit.Name}
	out := mergeManifests(&Manifest{Types: []TypeBinding{discovered}}, &Manifest{Types: []TypeBinding{explicit}})
	if len(out.Types) != 1 {
		t.Fatalf("one type in, %d out", len(out.Types))
	}
	got, want := reflect.ValueOf(out.Types[0]), reflect.ValueOf(explicit)
	for i := range want.NumField() {
		if !reflect.DeepEqual(got.Field(i).Interface(), want.Field(i).Interface()) {
			t.Errorf("TypeBinding.%s: explicit %v, merged %v", want.Type().Field(i).Name, want.Field(i), got.Field(i))
		}
	}

	// And per value: an explicit map overrides the values it names and keeps
	// the discovered rest.
	out = mergeManifests(
		&Manifest{Types: []TypeBinding{{Name: "S", Values: map[string]string{"A": "SA", "B": "SB"}}}},
		&Manifest{Types: []TypeBinding{{Name: "S", Values: map[string]string{"B": "SGone"}}}},
	)
	if v := out.Types[0].Values; v["A"] != "SA" || v["B"] != "SGone" {
		t.Errorf("Values merged to %v", v)
	}
}

// mergeManifests and yieldToDeclared each build a new Manifest by hand, so a
// field added to Manifest is dropped by both until they are taught it.
func TestManifestFieldsAreTheOnesTheMergeKnows(t *testing.T) {
	t.Parallel()
	var names []string
	for f := range reflect.TypeFor[Manifest]().Fields() {
		names = append(names, f.Name)
	}
	if want := []string{"Types", "ExtraEnums"}; !slices.Equal(names, want) {
		t.Errorf("Manifest has fields %v; mergeManifests and yieldToDeclared carry %v -- teach them the new one", names, want)
	}
}

// An explicit second type for the same Go type discovery found replaces it
// rather than registering it twice, which NewSchema refuses.
func TestExplicitExtraEnumReplacesTheDiscoveredOne(t *testing.T) {
	t.Parallel()
	gt := GoType{PkgPath: "hello/method", Name: "Method"}
	out := mergeManifests(
		&Manifest{ExtraEnums: []TypeBinding{{Name: "Method", Go: gt, Values: map[string]string{"A": "MA"}}}},
		&Manifest{ExtraEnums: []TypeBinding{{Name: "Method", Go: gt, Values: map[string]string{"A": "MOther"}}}},
	)
	if len(out.ExtraEnums) != 1 || out.ExtraEnums[0].Values["A"] != "MOther" {
		t.Errorf("ExtraEnums merged to %+v", out.ExtraEnums)
	}
}

// "The first pattern wins" is the documented precedence, and packages.Load
// returns roots in dependency order: with ent importing other, other came
// first whichever order the patterns were given in.
func TestTheFirstAutoBindPatternWinsWhenPackagesImportEachOther(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `type Product { id: ID! title: String! }
type Query { products: [Product!]! }
`
	const entSrc = `package ent

import "hello/other"

type Product struct {
	ID    string
	Title string
	Peer  *other.Product
}
`
	const otherSrc = "package other\n\ntype Product struct {\n\tID    string\n\tTitle string\n}\n"
	for _, tc := range []struct {
		patterns []string
		want     string
	}{
		{[]string{"./ent", "./other"}, "ent.Product"},
		{[]string{"./other", "./ent"}, "other.Product"},
		{[]string{"hello/ent", "./..."}, "ent.Product"},
		{[]string{"./oth...", "./ent"}, "other.Product"},
		{[]string{"./other/...", "./ent"}, "other.Product"},
	} {
		t.Run(strings.Join(tc.patterns, ","), func(t *testing.T) {
			dir := writeQualModule(t, sdl, entSrc, map[string]string{"other": otherSrc})
			src := autoGenerate(t, dir, Config{AutoBind: tc.patterns})
			if !strings.Contains(src, `graphql.Object[`+tc.want+`]("Product"`) {
				t.Errorf("AutoBind %v did not bind Product to %s\n%s", tc.patterns, tc.want, src)
			}
		})
	}
}

// Explicit second types are held to what newManifest holds a type to: an
// unknown enum binds nothing, silently, and partial values were a syntax error
// in generated.go. Both are refused, naming the enum, before a file is written.
func TestExplicitExtraEnumsAreValidated(t *testing.T) {
	t.Parallel()
	const sdl = `enum Method { STRAIGHT_LINE NONE }
input F { m: Method }
type Query { a(f: F): Method }
`
	gt := GoType{PkgPath: "hello/method", Name: "Method"}
	full := map[string]string{"STRAIGHT_LINE": "MethodStraightLine", "NONE": "MethodNone"}
	for _, tc := range []struct {
		name string
		tb   TypeBinding
		want []string
	}{
		{"partial", TypeBinding{Name: "Method", Go: gt, Values: map[string]string{"NONE": "MethodNone"}}, []string{"Method", "STRAIGHT_LINE"}},
		{"unknown enum", TypeBinding{Name: "Nope", Go: gt, Values: full}, []string{"Nope", "not defined"}},
		{"not an enum", TypeBinding{Name: "F", Go: gt, Values: full}, []string{"F", "an input object"}},
		{"no Go type", TypeBinding{Name: "Method", Values: full}, []string{"Method", "Go type"}},
		{"unknown value", TypeBinding{Name: "Method", Go: gt, Values: map[string]string{"STRAIGHT_LINE": "A", "NONE": "B", "GONE": "C"}}, []string{"Method", "GONE"}},
		{"bad constant", TypeBinding{Name: "Method", Go: gt, Values: map[string]string{"STRAIGHT_LINE": "A", "NONE": "not ident"}}, []string{"Method", "NONE"}},
		{"twice", TypeBinding{Name: "Method", Go: gt, Values: full}, []string{"Method", "more than once"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := []TypeBinding{tc.tb}
			if tc.name == "twice" {
				extra = append(extra, tc.tb)
			}
			dir, err := generateWith(t, sdl, Config{Manifest: &Manifest{ExtraEnums: extra}})
			if err == nil {
				t.Fatalf("%+v was accepted", tc.tb)
			}
			for _, w := range append(tc.want, "manifest: extra enum") {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error should mention %q: %v", w, err)
				}
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph")); serr == nil {
				t.Errorf("output was written before the refusal")
			}
		})
	}
}

// Input[T] matches an input field by its graphql or json tag or by its Go
// field's name, so codegen has no way to make @goField(name:) bind a field of
// an input it does not write. It was accepted and NewSchema then said
// "input field PlanIn.maxGb has no binding". A name the engine's own
// derivation already arrives at is coherent and stays accepted.
func TestFieldDirectiveNameOnABoundInputFieldIsRefused(t *testing.T) {
	t.Parallel()
	const decl = `
directive @goModel(model: String) on ENUM | OBJECT | INPUT_OBJECT
directive @goField(name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
`
	for _, via := range []struct {
		name string
		sdl  string
		cfg  Config
	}{
		{"model directive", `input PlanIn @goModel(model: "hello/ent.PlanIn") { maxGb: Int @goField(name: "MaxStorageGB") }`,
			Config{ModelDirective: ModelDirective{Name: "goModel", Arg: "model"}}},
		{"Models", `input PlanIn { maxGb: Int @goField(name: "MaxStorageGB") }`,
			Config{Models: map[string]string{"PlanIn": "hello/ent.PlanIn"}}},
		{"Manifest", `input PlanIn { maxGb: Int @goField(name: "MaxStorageGB") }`,
			Config{Manifest: &Manifest{Types: []TypeBinding{{Name: "PlanIn", Go: GoType{PkgPath: "hello/ent", Name: "PlanIn"}}}}}},
	} {
		t.Run(via.name, func(t *testing.T) {
			cfg := via.cfg
			cfg.FieldDirective = goFieldNamed
			_, err := generateWith(t, decl+via.sdl+"\ntype Query { plan(in: PlanIn): Int }\n", cfg)
			if err == nil {
				t.Fatal("@goField(name:) on a bound input's field was accepted")
			}
			for _, w := range []string{"PlanIn.maxGb", `graphql:"maxGb"`} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error should mention %s: %v", w, err)
				}
			}
		})
	}

	const coherent = `input PlanIn @goModel(model: "hello/ent.PlanIn") { authorId: ID @goField(name: "AuthorID") }
type Query { plan(in: PlanIn): Int }
`
	if _, err := generateWith(t, decl+coherent, Config{
		FieldDirective: goFieldNamed, ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Errorf("a name Input[T] derives the SDL name from was refused: %v", err)
	}
}

// An interface field is answered by each implementing object's field; the
// interface itself has no Go field or method for a name to choose, so the
// argument there named nothing.
func TestFieldDirectiveNameOnAnInterfaceFieldIsRefused(t *testing.T) {
	t.Parallel()
	const sdl = `
directive @goField(name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
interface Node { nodeId: ID! @goField(name: "Key") }
type User implements Node { nodeId: ID! }
type Query { node: Node }
`
	_, err := generateWith(t, sdl, Config{FieldDirective: goFieldNamed})
	if err == nil {
		t.Fatal("@goField(name:) on an interface field was accepted")
	}
	for _, w := range []string{"Node.nodeId", "User.nodeId"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error should mention %s: %v", w, err)
		}
	}
}

// A single-file federated schema takes the flat emitter, which no test
// generated: it has to compile and build as a subgraph.
func TestFlatFederatedSchemaBuildsAsASubgraph(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping go run subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/schema.graphql": "type User @key(fields: \"id\") { id: ID! name: String! }\ntype Query { me: User }\n",
		"check/main.go":         "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\t\"hello/graph\"\n)\n\nfunc main() {\n\tif err := graph.ValidateSchema(); err != nil {\n\t\tfmt.Println(err)\n\t\tos.Exit(1)\n\t}\n}\n",
	})
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph", Federation: true,
	}); err != nil {
		t.Fatal(err)
	}
	src := readFile(t, filepath.Join(dir, "graph", "schema.go"))
	if !strings.Contains(src, "func NewSchema(r Resolver, entities []fed.Entity, opts ...graphql.SchemaOption)") {
		t.Fatalf("schema.go is not the flat federated one:\n%s", src)
	}
	goBuild(t, dir)
	run := exec.Command("go", "run", "./check")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("ValidateSchema on the flat subgraph: %v\n%s", err, out)
	}
}
