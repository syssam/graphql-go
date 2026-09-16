package domain

import "testing"

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
