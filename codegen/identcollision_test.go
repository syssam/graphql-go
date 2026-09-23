package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateSDL runs Generate over one SDL file in a fresh directory.
func generateSDL(t *testing.T, sdl string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema", "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
	})
	return dir, err
}

// Legal SDL whose generated Go does not compile: two fields whose Go names
// fold together, and a type whose Go name is unexported. The generator used
// to write it anyway, so the failure arrived as `ID redeclared` or `name
// color not exported by package model` inside a DO NOT EDIT file. It must be
// refused before anything is written, naming the SDL coordinates.
func TestUncompilableIdentifiersAreRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		sdl  string
		want []string
	}{
		{
			name: "object fields",
			sdl:  `type User { id: ID! ID: String! } type Query { u: User }`,
			want: []string{"User.id", "User.ID", `"ID"`},
		},
		{
			name: "input fields",
			sdl:  `input Filter { userId: ID userID: ID } type Query { q(f: Filter): String }`,
			want: []string{"Filter.userId", "Filter.userID", `"UserID"`},
		},
		{
			name: "arguments",
			sdl:  `type Query { q(userId: ID, userID: ID): String }`,
			want: []string{"Query.q(userId:)", "Query.q(userID:)", `"UserID"`},
		},
		{
			name: "enum values",
			sdl:  `enum Level { Low LOW } type Query { l: Level }`,
			want: []string{"Level.Low", "Level.LOW", `"LevelLow"`},
		},
		{
			name: "lower-case type",
			sdl:  `enum color { RED } type Query { c: color }`,
			want: []string{`"color"`, "Models"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, err := generateSDL(t, c.sdl)
			if err == nil {
				t.Fatal("a schema whose generated code cannot compile was accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the error does not name %s:\n%v", w, err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(dir, "graph")); statErr == nil {
				t.Error("output was written despite the refusal")
			}
		})
	}
}

// The check must not refuse what compiles: fields that differ only by a
// leading underscore stay apart (X prefix), and so do id and ownerId.
func TestDistinctIdentifiersAreAccepted(t *testing.T) {
	if _, err := generateSDL(t, `type User { id: ID! _id: ID ownerId: ID owner: String } type Query { u: User }`); err != nil {
		t.Fatal(err)
	}
}
