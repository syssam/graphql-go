package graphql

import (
	"strings"
	"testing"
)

type orderRole int

// Enum reported the first bad value it met while ranging its map, so the same
// mistake gave a different message run to run and named one value of several.
func TestEnumBuildErrorsNameEveryValueInOneOrder(t *testing.T) {
	build := func() string {
		_, err := NewSchema(SDL(`enum Role { A } type Query { r: Role }`),
			Enum("Role", map[orderRole]string{0: "A", 1: "Z", 2: "X", 3: "Y"}),
			Query(Field("r", func(Root) *orderRole { return nil })),
		)
		if err == nil {
			t.Fatal("an enum mapping to values the schema does not define was accepted")
		}
		return err.Error()
	}
	first := build()
	for _, v := range []string{"X", "Y", "Z"} {
		if !strings.Contains(first, `"`+v+`"`) {
			t.Errorf("error does not name %s: %s", v, first)
		}
	}
	for range 40 {
		if got := build(); got != first {
			t.Fatalf("the same schema gave two build errors:\n%s\n%s", first, got)
		}
	}
}

func TestEnumDuplicateMappingsAreReportedInOneOrder(t *testing.T) {
	build := func() string {
		_, err := NewSchema(SDL(`enum Role { A B } type Query { r: Role }`),
			Enum("Role", map[orderRole]string{0: "A", 1: "A", 2: "B", 3: "B"}),
			Query(Field("r", func(Root) *orderRole { return nil })),
		)
		if err == nil {
			t.Fatal("two Go values mapped to one enum value were accepted")
		}
		return err.Error()
	}
	first := build()
	for range 40 {
		if got := build(); got != first {
			t.Fatalf("the same schema gave two build errors:\n%s\n%s", first, got)
		}
	}
}
