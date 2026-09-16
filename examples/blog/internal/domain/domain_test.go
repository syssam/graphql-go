package domain

import (
	"go/build"
	"strings"
	"testing"
)

// TestDomainImportsNothingButStdlib guards what the package comment claims. A
// contributor who writes graphql.ID on domain.User breaks the example's entire
// point, and every test in every module stays green while they do it.
func TestDomainImportsNothingButStdlib(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "graphql-go") {
			t.Fatalf("domain imports %q; the SDL must not be able to reach this package", imp)
		}
	}
}

func TestOptionalDistinguishesAbsentFromNull(t *testing.T) {
	absent := Absent[string]()
	if absent.Present {
		t.Fatal("Absent reported itself as present")
	}

	cleared := Present[string](nil)
	if !cleared.Present || cleared.Value != nil {
		t.Fatalf("Present(nil) must be present and hold nil, got %+v", cleared)
	}

	v := "Final"
	set := Present(&v)
	if !set.Present || set.Value == nil || *set.Value != "Final" {
		t.Fatalf("Present(&v) must carry the value, got %+v", set)
	}
}
