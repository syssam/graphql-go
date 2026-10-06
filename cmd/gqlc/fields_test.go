package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/codegen"
)

const fieldsHead = "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n"

const fieldsSDL = `
type Employee { id: ID! name: String! salary: Int }
type Query { employees: [Employee!]! }
`

// The key this exists for: a field a service gates has to be answered by a
// Resolver method, and without the key a scalar is read straight off the
// struct.
func TestFieldsResolverForcesAResolverMethod(t *testing.T) {
	dir, cfg := write(t, fieldsHead+"fields:\n  Employee:\n    salary: { resolver: true }\n", fieldsSDL)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	src := generated(t, dir, "graph/generated.go")
	for _, want := range []string{
		`graphql.Resolve("salary"`,
		"EmployeeSalary(ctx context.Context, obj *model.Employee) (*int, error)",
		`graphql.Field("name"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go lacks %s:\n%s", want, src)
		}
	}
	// And a struct read without the key, or the assertions above prove nothing.
	dir2, cfg2 := write(t, fieldsHead, fieldsSDL)
	if err := run([]string{"-config", cfg2}); err != nil {
		t.Fatal(err)
	}
	if src := generated(t, dir2, "graph/generated.go"); !strings.Contains(src, `graphql.Field("salary"`) || strings.Contains(src, "EmployeeSalary") {
		t.Fatalf("salary was a resolver without the fields key:\n%s", src)
	}
}

// fields is the manifest a wrapper program used to build from two files of its
// own, so it has to become the same manifest: types in order, a zero Go type
// that leaves the type's own binding alone, and nothing set that the entry
// did not say.
func TestFieldsBecomeTheManifest(t *testing.T) {
	fc := fileConfig{Fields: map[string]map[string]fieldEntry{
		"Invoice": {
			"total": {Name: "Total", Method: true},
			"note":  {Name: "Note", Value: true},
		},
		"Employee":    {"salary": {Resolver: true}, "hourlyRate": {Resolver: true}},
		"AiMcpServer": {"url": {Name: "URL"}},
	}}
	got, err := fc.manifest()
	if err != nil {
		t.Fatal(err)
	}
	want := &codegen.Manifest{Types: []codegen.TypeBinding{
		{Name: "AiMcpServer", Fields: map[string]codegen.FieldBinding{
			"url": {Kind: codegen.FieldStruct, GoName: "URL"},
		}},
		{Name: "Employee", Fields: map[string]codegen.FieldBinding{
			"salary":     {Kind: codegen.FieldResolver},
			"hourlyRate": {Kind: codegen.FieldResolver},
		}},
		{Name: "Invoice", Fields: map[string]codegen.FieldBinding{
			"total": {Kind: codegen.FieldMethod, GoName: "Total"},
			"note":  {Kind: codegen.FieldStruct, GoName: "Note", Value: true},
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest = %+v\nwant       %+v", got, want)
	}
	if got := fc.forcedResolvers(); !reflect.DeepEqual(got, []string{"Employee.hourlyRate", "Employee.salary"}) {
		t.Errorf("forcedResolvers = %v", got)
	}
	// No key, no manifest: a manifest that is merely empty is still one, and
	// the generator must be left exactly as it was without the key.
	if m, err := (&fileConfig{}).manifest(); m != nil || err != nil {
		t.Errorf("without fields the manifest is %+v, %v; want none", m, err)
	}
}

// fieldsModule is a module holding a hand-written type whose Go names are not
// the ones gqlc derives, bound through models with no autoBind to read it: the
// case name, method and value are for.
func fieldsModule(t *testing.T, fields string) (dir, cfg string) {
	t.Helper()
	dir = t.TempDir()
	for rel, body := range map[string]string{
		"ext/server.go": `package ext

type Server struct {
	Name  string
	URL   string
	Notes string
}

func (s *Server) Label() string { return s.Name }
`,
		"schema.graphql": "type AiMcpServer { name: String! url: String! label: String! note: String }\ntype Query { servers: [AiMcpServer!]! }\n",
		"gqlc.yaml":      "schema: [schema.graphql]\noutput: graph\npackage: hello/graph\nmodels:\n  AiMcpServer: hello/ext.Server\n" + fields,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, filepath.Join(dir, "gqlc.yaml")
}

const serverFields = `fields:
  AiMcpServer:
    url: { name: URL }
    label: { name: Label, method: true }
    note: { name: Notes, value: true }
`

func TestFieldsNameMethodAndValueBindTheGoSide(t *testing.T) {
	dir, cfg := fieldsModule(t, serverFields)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	src := generated(t, dir, "graph/generated.go")
	for what, want := range map[string]string{
		"name":   `graphql.Field("url", func(v *ext.Server) string { return v.URL })`,
		"method": `graphql.Field("label", func(v *ext.Server) string { return v.Label() })`,
		"value":  `graphql.Field("note", func(v *ext.Server) string { return v.Notes })`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s: generated.go lacks %s:\n%s", what, want, src)
		}
	}
	if strings.Contains(src, "type Resolver interface {\n\t// AiMcpServer") {
		t.Errorf("a bound field was also asked of the Resolver:\n%s", src)
	}

	// Without the key each is written from the SDL's name, which the struct
	// does not have.
	dir2, cfg2 := fieldsModule(t, "")
	if err := run([]string{"-config", cfg2}); err != nil {
		t.Fatal(err)
	}
	src = generated(t, dir2, "graph/generated.go")
	for _, want := range []string{"return v.Url }", "return v.Label }", "*string { return v.Note }"} {
		if !strings.Contains(src, want) {
			t.Errorf("without fields, generated.go lacks %s:\n%s", want, src)
		}
	}
}

// What the bindings are for is code that compiles against a struct gqlc did
// not write, so the output is compiled against it.
func TestFieldsOutputCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, cfg := fieldsModule(t, serverFields)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	mod := "module hello\n\ngo 1.27\n\nrequire github.com/syssam/graphql-go v0.0.0\n\nreplace github.com/syssam/graphql-go => " + filepath.ToSlash(root) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// Each entry that cannot be meant is refused before anything is generated,
// naming the type and field: on a list that exists so a gated field cannot be
// read past its gate, an entry that quietly meant something else is the
// failure.
func TestFieldsEntriesThatCannotBeMeant(t *testing.T) {
	for name, c := range map[string]struct {
		entry string
		want  []string
	}{
		"resolver with name":           {"salary: { resolver: true, name: Salary }", []string{"fields: Employee.salary:", "resolver: true stands alone", "name"}},
		"resolver with method":         {"salary: { resolver: true, method: true }", []string{"fields: Employee.salary:", "resolver: true stands alone", "method"}},
		"resolver with value":          {"salary: { resolver: true, value: true }", []string{"fields: Employee.salary:", "resolver: true stands alone", "value"}},
		"method with value":            {"salary: { name: Salary, method: true, value: true }", []string{"fields: Employee.salary:", "method and value are both set"}},
		"method without name":          {"salary: { method: true }", []string{"fields: Employee.salary:", "method: true needs name"}},
		"value without name":           {"salary: { value: true }", []string{"fields: Employee.salary:", "value: true needs name"}},
		"an entry that says nothing":   {"salary: {}", []string{"fields: Employee.salary:", "says nothing"}},
		"an entry left empty":          {"salary:", []string{"fields: Employee.salary:", "says nothing"}},
		"resolver: false alone":        {"salary: { resolver: false }", []string{"fields: Employee.salary:", "says nothing"}},
		"an unexported name":           {"salary: { name: salary }", []string{"fields: Employee.salary:", `name "salary" is not an exported Go identifier`}},
		"a name that is no identifier": {"salary: { name: Pay.Salary }", []string{"fields: Employee.salary:", `name "Pay.Salary" is not an exported Go identifier`}},
		"a misspelled key":             {"salary: { resolvr: true }", []string{"resolvr", "gqlc.yaml"}},
		"a list where the entry goes":  {"salary: [resolver]", []string{"gqlc.yaml", "parse"}},
		// The same field forced and bound: neither reading may win quietly.
		"a field written twice": {"salary: { resolver: true }\n    salary: { name: Salary }", []string{"gqlc.yaml", `"salary" already defined`}},
		// Every mistake at once, so fixing a list is one run and not one per entry.
		"two entries": {"salary: {}\n    name: { value: true }", []string{"fields: Employee.salary:", "fields: Employee.name:", "value: true needs name"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir, cfg := write(t, fieldsHead+"fields:\n  Employee:\n    "+c.entry+"\n", fieldsSDL)
			err := run([]string{"-config", cfg})
			if err == nil {
				t.Fatal("the entry was accepted")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q:\n%v", want, err)
				}
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph")); serr == nil {
				t.Error("the output was written before the entry was refused")
			}
		})
	}

	// A type with nothing under it is what a mis-indented list parses as.
	t.Run("a type that names no field", func(t *testing.T) {
		_, cfg := write(t, fieldsHead+"fields:\n  Employee:\n", fieldsSDL)
		err := run([]string{"-config", cfg})
		if err == nil || !strings.Contains(err.Error(), "fields: Employee names no field") {
			t.Fatalf("err = %v", err)
		}
	})
}

// A type or field the schema does not have is a typo, and on this list a typo
// that did nothing leaves a gated field bound to its struct. The generator
// refuses a manifest naming either; this pins that it does so through the key,
// and what it says.
func TestFieldsNamingWhatTheSchemaLacks(t *testing.T) {
	for name, c := range map[string]struct{ fields, want string }{
		"an unknown type":           {"Emploee:\n    salary: { resolver: true }", "codegen: manifest: type Emploee is not defined in the schema"},
		"an unknown field":          {"Employee:\n    salry: { resolver: true }", "codegen: manifest: field Employee.salry is not defined in the schema"},
		"an unknown field, by name": {"Employee:\n    salry: { name: Salary }", "codegen: manifest: field Employee.salry is not defined in the schema"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, cfg := write(t, fieldsHead+"fields:\n  "+c.fields+"\n", fieldsSDL)
			err := run([]string{"-config", cfg})
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v\nwant  %s", err, c.want)
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph")); serr == nil {
				t.Error("the output was written all the same")
			}
		})
	}
}

const interfaceSDL = `
interface Node { id: ID! }
type Employee implements Node { id: ID! salary: Int }
type Query { employees: [Employee!]! }
`

// A field of an interface has no resolver to be forced to. The generator
// refuses the entry before anything is written, naming it: accepted, it did
// nothing, and the object's own field stayed a struct read behind a gate the
// author believed was there. The check after a generate is the second guard
// on the same list, and TestCheckForcedResolvers drives it directly.
func TestFieldsRefusesAnEntryThatCanBindNothing(t *testing.T) {
	yml := fieldsHead + "fields:\n  Node:\n    id: { resolver: true }\n  Employee:\n    salary: { resolver: true }\n"
	dir, cfg := write(t, yml, interfaceSDL)
	err := run([]string{"-config", cfg})
	if err == nil {
		t.Fatalf("an entry on an interface's field passed:\n%s", generated(t, dir, "graph/generated.go"))
	}
	for _, want := range []string{"Node.id", "Node is an interface"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "graph", "generated.go")); serr == nil {
		t.Error("output was written for a configuration that was refused")
	}
	// A check refuses what a generate would.
	if cerr := run([]string{"-config", cfg, "-check"}); cerr == nil || !strings.Contains(cerr.Error(), "Node.id") {
		t.Errorf("-check over the same configuration said %v", cerr)
	}

	// And with the entry on the object, both pass.
	yml = fieldsHead + "fields:\n  Employee:\n    id: { resolver: true }\n    salary: { resolver: true }\n"
	_, cfg = write(t, yml, interfaceSDL)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-config", cfg, "-check"}); err != nil {
		t.Fatalf("-check over a generate that passed: %v", err)
	}
}

// The check reads what the generator wrote, so it is driven here with files
// written by hand: every way a forced field can fail to be one, and the three
// places a group's generated.go can be.
func TestCheckForcedResolvers(t *testing.T) {
	const header = "// Code generated by gqlc. DO NOT EDIT.\n\npackage employee\n\n"
	const good = header + `type Resolver interface {
	// EmployeeSalary resolves Employee.salary.
	//
	// What the employee is paid.
	EmployeeSalary(ctx context.Context, obj *Employee) (*int, error)
}

func Bindings(r Resolver) []graphql.SchemaOption {
	return graphql.Resolve("salary", func(ctx context.Context, v *Employee) (*int, error) { return r.EmployeeSalary(ctx, v) })
}
`
	forced := []string{"Employee.salary"}
	lay := func(t *testing.T, files map[string]string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "graph")
		for rel, body := range files {
			p := filepath.Join(out, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}

	for name, c := range map[string]struct {
		groupDir string
		files    map[string]string
	}{
		"one group":           {"", map[string]string{"generated.go": good}},
		"a package per group": {"", map[string]string{"employee/generated.go": good, "invoice/generated.go": header}},
		"under groupDir":      {"register/api", map[string]string{"register/api/employee/generated.go": good}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkForcedResolvers(lay(t, c.files), c.groupDir, forced); err != nil {
				t.Fatal(err)
			}
		})
	}

	for name, c := range map[string]struct {
		groupDir string
		files    map[string]string
	}{
		"nothing generated": {"", nil},
		"a struct read": {"", map[string]string{"employee/generated.go": header + `type Resolver interface {
	// EmployeeName resolves Employee.name.
	EmployeeName(ctx context.Context, obj *Employee) (string, error)
}

var _ = graphql.Field("salary", func(v *Employee) *int { return v.Salary })
`}},
		// The same words, where they are not the line the generator writes
		// over a Resolver method.
		"the line in a description": {"", map[string]string{"employee/generated.go": header + `type Resolver interface {
	// EmployeeName resolves Employee.name.
	//
	// EmployeeName resolves Employee.salary.
	EmployeeName(ctx context.Context, obj *Employee) (string, error)
}

var _ = func(r Resolver) { r.EmployeeName(nil, nil) }
`}},
		"another method's line": {"", map[string]string{"employee/generated.go": header + `type Resolver interface {
	// EmployeeSalary resolves Employee.salary.
	EmployeeName(ctx context.Context, obj *Employee) (string, error)
}

var _ = func(r Resolver) { r.EmployeeName(nil, nil) }
`}},
		"an interface that is not the Resolver": {"", map[string]string{"employee/generated.go": header + `type Legacy interface {
	// EmployeeSalary resolves Employee.salary.
	EmployeeSalary(ctx context.Context, obj *Employee) (*int, error)
}

var _ = func(r Legacy) { r.EmployeeSalary(nil, nil) }
`}},
		// On the interface, and the binding reads the struct all the same.
		"a method nothing calls": {"", map[string]string{"employee/generated.go": header + `type Resolver interface {
	// EmployeeSalary resolves Employee.salary.
	EmployeeSalary(ctx context.Context, obj *Employee) (*int, error)
}

var _ = graphql.Field("salary", func(v *Employee) *int { return v.Salary })
`}},
		// Another run's output nested in this one's is not a group of this
		// run, whatever its Resolver has.
		"another gqlc run's output": {"", map[string]string{
			"legacy/generated.go":          good,
			"legacy/schema.go":             "// Code generated by gqlc. DO NOT EDIT.\n\npackage legacy\n",
			"legacy/schema/schema.graphql": "type Employee { salary: Int }\n",
		}},
		"outside groupDir":             {"register", map[string]string{"employee/generated.go": good}},
		"a file that is not a group's": {"", map[string]string{"employee/handler.go": good}},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkForcedResolvers(lay(t, c.files), c.groupDir, forced)
			if err == nil || !strings.Contains(err.Error(), "Employee.salary") || !strings.Contains(err.Error(), "fields: 1 of 1") {
				t.Fatalf("err = %v, want one naming Employee.salary", err)
			}
		})
	}

	// A group package may hold a schema.go and a schema/ of its own; only a
	// generated schema.go beside one makes it another run's.
	t.Run("a group with its own schema directory", func(t *testing.T) {
		out := lay(t, map[string]string{
			"employee/generated.go":   good,
			"employee/schema.go":      "package employee\n",
			"employee/schema/doc.txt": "notes\n",
		})
		if err := checkForcedResolvers(out, "", forced); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a file that does not parse is an error, not a pass", func(t *testing.T) {
		out := lay(t, map[string]string{"employee/generated.go": good + "}\n"})
		err := checkForcedResolvers(out, "", forced)
		if err == nil || !strings.Contains(err.Error(), "generated.go") || strings.Contains(err.Error(), "1 of 1") {
			t.Fatalf("err = %v, want the parse error naming the file", err)
		}
	})

	t.Run("nothing forced reads nothing", func(t *testing.T) {
		if err := checkForcedResolvers(filepath.Join(t.TempDir(), "absent"), "", nil); err != nil {
			t.Fatal(err)
		}
	})
}

// The check holds across groups and groupDir as the generator lays them out,
// which is what ties the hand-written files above to real output.
func TestFieldsResolverCheckFollowsTheLayout(t *testing.T) {
	for name, extra := range map[string]string{
		"a package per group": "",
		"under groupDir":      "groupDir: register\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, body := range map[string]string{
				"employee.graphql": "type Employee { id: ID! salary: Int }\n",
				"schema.graphql":   "type Query { employees: [Employee!]! version: String! }\n",
				"gqlc.yaml":        "schema: ['*.graphql']\noutput: graph\npackage: example/graph\n" + extra + "fields:\n  Employee:\n    salary: { resolver: true }\n  Query:\n    version: { resolver: true }\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := run([]string{"-config", filepath.Join(dir, "gqlc.yaml")}); err != nil {
				t.Fatal(err)
			}
			rel := "graph/employee/generated.go"
			if extra != "" {
				rel = "graph/register/employee/generated.go"
			}
			if src := generated(t, dir, rel); !strings.Contains(src, "// EmployeeSalary resolves Employee.salary.") {
				t.Fatalf("the layout is not the one this test is about:\n%s", src)
			}
		})
	}
}
