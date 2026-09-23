package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write lays out a config and schema in a temp dir and returns the config path.
func write(t *testing.T, yml, sdl string) (dir, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	if sdl != "" {
		if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "gqlc.yaml")
	if err := os.WriteFile(cfgPath, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, cfgPath
}

func generated(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// Every yaml key is plumbing between two structs, and a misspelled tag fails
// silently: the option is simply ignored and the generator does the default
// thing. Each of these asserts the key reached codegen by an effect visible in
// the output, not by reading the struct back.
func TestConfigKeysReachTheGenerator(t *testing.T) {
	const sdl = `
input Filter { name: String }
type User { id: ID! }
type Query { users(f: Filter): [User!]! }
`
	t.Run("nullableInputOmittable", func(t *testing.T) {
		dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nnullableInputOmittable: true\n", sdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		src := generated(t, dir, "graph/model/models.go")
		if !strings.Contains(src, "Omittable[") {
			t.Fatalf("nullableInputOmittable did not reach the generator; models.go has no Omittable:\n%s", src)
		}
	})

	t.Run("off by default", func(t *testing.T) {
		dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n", sdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir, "graph/model/models.go"); strings.Contains(src, "Omittable[") {
			t.Fatal("Omittable appeared without nullableInputOmittable being set")
		}
	})

	t.Run("output and package", func(t *testing.T) {
		dir, cfg := write(t, "schema: [schema.graphql]\noutput: gen\npackage: example.com/app/gen\n", sdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		src := generated(t, dir, "gen/generated.go")
		if !strings.Contains(src, "package gen") {
			t.Fatalf("output directory did not reach the generator:\n%s", src[:200])
		}
	})

	t.Run("models binds an external type", func(t *testing.T) {
		dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nmodels:\n  User: example.com/ext.User\n", sdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		src := generated(t, dir, "graph/generated.go")
		if !strings.Contains(src, "example.com/ext") {
			t.Fatalf("models did not reach the generator; generated.go does not mention the bound package:\n%s", src)
		}
	})

	t.Run("modelDirective reads a binding off the SDL", func(t *testing.T) {
		const dsdl = `
directive @goModel(model: String) on OBJECT
type User @goModel(model: "example.com/ext.User") { id: ID! }
type Query { users: [User!]! }
`
		dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nmodelDirective:\n  name: goModel\n  arg: model\n", dsdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir, "graph/generated.go"); !strings.Contains(src, "example.com/ext") {
			t.Fatalf("modelDirective did not reach the generator:\n%s", src)
		}
	})

	t.Run("zeroForNullInputs", func(t *testing.T) {
		const zsdl = `
input Filter { isNil: Boolean }
type Query { q(f: Filter): String! }
`
		yml := "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nzeroForNullInputs: true\n"
		dir, cfg := write(t, yml, zsdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir, "graph/generated.go"); !strings.Contains(src, "graphql.ZeroForNull()") {
			t.Fatalf("zeroForNullInputs did not reach the generator:\n%s", src)
		}
		// Off without the key, or the assertion above proves nothing.
		dir2, cfg2 := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n", zsdl)
		if err := run([]string{"-config", cfg2}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir2, "graph/generated.go"); strings.Contains(src, "graphql.ZeroForNull()") {
			t.Fatalf("ZeroForNull appeared without the key:\n%s", src)
		}
	})

	t.Run("fieldDirective forces a resolver", func(t *testing.T) {
		const fsdl = `
directive @goField(forceResolver: Boolean) on FIELD_DEFINITION
type User { id: ID! nickname: String! @goField(forceResolver: true) }
type Query { users: [User!]! }
`
		yml := "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nfieldDirective:\n  name: goField\n  forceResolverArg: forceResolver\n"
		dir, cfg := write(t, yml, fsdl)
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir, "graph/generated.go"); !strings.Contains(src, `graphql.Resolve("nickname"`) {
			t.Fatalf("fieldDirective did not reach the generator; nickname is not a resolver:\n%s", src)
		}
		// And off without the key, or the assertion above proves nothing.
		dir2, cfg2 := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n", fsdl)
		if err := run([]string{"-config", cfg2}); err != nil {
			t.Fatal(err)
		}
		if src := generated(t, dir2, "graph/generated.go"); !strings.Contains(src, `graphql.Field("nickname"`) {
			t.Fatalf("nickname was a resolver without fieldDirective being set:\n%s", src)
		}
	})
}

// A CLI's error messages are its whole interface when something is wrong, so
// each of these asserts the message names the thing that failed, not just that
// an error came back.
func TestRunErrors(t *testing.T) {
	t.Run("unknown flag", func(t *testing.T) {
		if err := run([]string{"-nope"}); err == nil {
			t.Fatal("an unknown flag was accepted")
		}
	})

	t.Run("missing config names the path", func(t *testing.T) {
		err := run([]string{"-config", filepath.Join(t.TempDir(), "absent.yaml")})
		if err == nil {
			t.Fatal("a missing config was accepted")
		}
		if !strings.Contains(err.Error(), "absent.yaml") {
			t.Fatalf("error does not name the config path: %v", err)
		}
	})

	t.Run("malformed yaml names the path", func(t *testing.T) {
		_, cfg := write(t, "schema: [unclosed\n", "")
		err := run([]string{"-config", cfg})
		if err == nil {
			t.Fatal("malformed yaml was accepted")
		}
		if !strings.Contains(err.Error(), "gqlc.yaml") {
			t.Fatalf("error does not name the config path: %v", err)
		}
	})

	t.Run("misspelled key is refused", func(t *testing.T) {
		_, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nnullableInputOmitable: true\n",
			"type Query { a: String }\n")
		err := run([]string{"-config", cfg})
		if err == nil {
			t.Fatal("a misspelled key was silently ignored")
		}
		if !strings.Contains(err.Error(), "nullableInputOmitable") {
			t.Fatalf("error does not name the unknown key: %v", err)
		}
	})

	t.Run("generator failure is reported", func(t *testing.T) {
		_, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n",
			"type Query { broken: DoesNotExist }\n")
		if err := run([]string{"-config", cfg}); err == nil {
			t.Fatal("an unresolvable schema was accepted")
		}
	})

	t.Run("default config path is gqlc.yaml", func(t *testing.T) {
		// No -config: the flag default must be gqlc.yaml, and the error must
		// say so, or a user in the wrong directory gets no hint.
		err := run(nil)
		if err == nil {
			t.Skip("a gqlc.yaml exists in the working directory")
		}
		if !strings.Contains(err.Error(), "gqlc.yaml") {
			t.Fatalf("error does not name the default config: %v", err)
		}
	})
}
