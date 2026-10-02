package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func namedFrom(t *testing.T, src, name string) *types.Named {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("p", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pkg.Scope().Lookup(name).Type().(*types.Named)
}

// A name promoted from two embedded structs at one depth is not a field of
// the outer struct: Go refuses the selector as ambiguous. Discovery kept the
// first it met and bound the GraphQL field to v.Name, in a file marked DO NOT
// EDIT that then did not compile.
func TestStructFieldsLeavesOutAnAmbiguousPromotion(t *testing.T) {
	fields := structFields(namedFrom(t, `package p
		type A struct{ Name string; OnlyA int }
		type B struct{ Name string }
		type Thing struct { A; B; ID string }
		type Shadow struct { A; B; Name string }
	`, "Thing"))
	if _, ok := fields["name"]; ok {
		t.Error("Thing.Name is ambiguous between A and B, and was offered as a field")
	}
	for _, want := range []string{"id", "onlya"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("%s is an ordinary field of Thing and is missing", want)
		}
	}
}

// A field of the struct itself hides both, which is Go's rule too.
func TestStructFieldsKeepsAShallowerFieldOverAnAmbiguousPair(t *testing.T) {
	fields := structFields(namedFrom(t, `package p
		type A struct{ Name string }
		type B struct{ Name string }
		type Shadow struct { A; B; Name string }
	`, "Shadow"))
	if f, ok := fields["name"]; !ok || f.deep != 0 {
		t.Errorf("Shadow.Name is its own field at depth 0; got %+v (present=%t)", f, ok)
	}
}

// json:"-" is how a struct says a field is not for the wire, and how an ORM
// marks a sensitive column: ent writes it on every Sensitive() field. The
// engine's Input[T] honours it. Discovery read the tag as "no name", fell back
// to the Go name, and bound Password to a `password` field in the schema.
func TestStructFieldsLeavesOutAFieldHiddenFromJSON(t *testing.T) {
	fields := structFields(namedFrom(t, "package p\ntype U struct {\n"+
		"\tPassword string `json:\"-\"`\n"+
		"\tName string `json:\"name\"`\n"+
		"\tDash string `json:\"-,\"`\n"+
		"}", "U"))
	if _, ok := fields["password"]; ok {
		t.Error("a field tagged json:\"-\" was offered for binding")
	}
	if _, ok := fields["name"]; !ok {
		t.Error("an ordinary tagged field is missing")
	}
	// encoding/json's escape for a field really named "-".
	if f, ok := fields["dash"]; !ok || f.tag != "-" {
		t.Errorf("json:\"-,\" names the field \"-\"; got %+v (present=%t)", f, ok)
	}
}

// Two fields can carry one json tag, and the match ranged a map: which of them
// answered the GraphQL field changed from one generate to the next.
func TestMatchFieldIsTheSameEveryTime(t *testing.T) {
	fields := map[string]goField{
		"alpha": {name: "Alpha", tag: "id"},
		"beta":  {name: "Beta", tag: "id"},
		"gamma": {name: "Gamma", tag: "id"},
	}
	first, ok := matchField(fields, "id")
	if !ok {
		t.Fatal("no match")
	}
	for range 100 {
		if got, _ := matchField(fields, "id"); got.name != first.name {
			t.Fatalf("matchField answered %s and then %s for the same input", first.name, got.name)
		}
	}
}

// Two more places the tag has to be honoured. An embedded struct tagged
// json:"-" hides everything it would promote. And a hidden field still
// shadows a deeper one of its name: Go resolves v.Secret to the outer field,
// so offering the embedded one binds the GraphQL field to the hidden value.
func TestStructFieldsHonoursTheHiddenTagThroughEmbedding(t *testing.T) {
	src := "package p\n" +
		"type Credentials struct{ PasswordHash string }\n" +
		"type Base struct{ Secret string; Plain string }\n" +
		"type User struct {\n" +
		"\tCredentials `json:\"-\"`\n" +
		"\tBase\n" +
		"\tSecret string `json:\"-\"`\n" +
		"\tName string\n" +
		"}"
	fields := structFields(namedFrom(t, src, "User"))
	if _, ok := fields["passwordhash"]; ok {
		t.Error("a field promoted from an embedded struct tagged json:\"-\" was offered")
	}
	if f, ok := fields["secret"]; ok {
		t.Errorf("Secret is hidden at depth 0 and shadows Base.Secret; got %+v", f)
	}
	for _, want := range []string{"name", "plain"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("%s is an ordinary field and is missing", want)
		}
	}
}
