package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Resolver method is its type name and field name run together, so two
// different fields can land on one name. This pair is taken from a real
// 5 503-type schema, where it made the generator emit an interface declaring
// FeatureFlagTargetingRules twice -- 276 000 lines of output whose only
// failure was a compile error inside a file marked DO NOT EDIT, naming a line
// the author cannot act on.
//
// Failing at generation instead, naming both coordinates, is the whole fix.
func TestResolverMethodCollisionIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	const sdl = `
type FeatureFlag { id: ID! targetingRules: FeatureFlagTargeting }
type FeatureFlagTargeting { rules: [FeatureFlagRule!] }
type FeatureFlagRule { id: ID! }
type Query { flag(id: ID!): FeatureFlag }
`
	if err := os.WriteFile(filepath.Join(dir, "schema", "featureflag.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
	})
	if err == nil {
		t.Fatal("a schema whose generated code cannot compile was accepted")
	}
	for _, want := range []string{
		"FeatureFlag.targetingRules",
		"FeatureFlagTargeting.rules",
		`"FeatureFlagTargetingRules"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s:\n%v", want, err)
		}
	}

	// Nothing may be written: a half-generated tree is worse than none.
	if _, statErr := os.Stat(filepath.Join(dir, "graph")); statErr == nil {
		t.Error("output was written despite the collision")
	}
}

// The interface is per group, so the same two types in different groups are
// fine -- and that is the cheapest remedy, which the error message names.
func TestSameCollisionInDifferentGroupsIsFine(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, "schema", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("flag.graphql", "type FeatureFlag { id: ID! targetingRules: FeatureFlagTargeting }\ntype Query { flag(id: ID!): FeatureFlag }\n")
	write("targeting.graphql", "type FeatureFlagTargeting { rules: [FeatureFlagRule!] }\ntype FeatureFlagRule { id: ID! }\n")

	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
	}); err != nil {
		t.Fatalf("the same two fields in different groups were refused: %v", err)
	}
}
