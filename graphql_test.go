package graphql

import (
	"encoding/json"
	"testing"
)

func TestOmittableZeroValue(t *testing.T) {
	var o Omittable[*string]
	if o.IsSet() {
		t.Fatal("zero Omittable must not be set")
	}
	if o.Value() != nil {
		t.Fatal("zero Omittable must hold a nil value")
	}
	if v, ok := o.ValueOK(); ok || v != nil {
		t.Fatalf("ValueOK() = (%v, %v), want (nil, false)", v, ok)
	}
}

func TestOmittableOf(t *testing.T) {
	s := "x"
	o := OmittableOf(&s)
	if !o.IsSet() || o.Value() != &s {
		t.Fatal("OmittableOf must be set and carry the value")
	}

	n := OmittableOf[*string](nil)
	if !n.IsSet() || n.Value() != nil {
		t.Fatal("OmittableOf(nil) must be set with a nil value")
	}
	if v, ok := n.ValueOK(); !ok || v != nil {
		t.Fatalf("ValueOK() = (%v, %v), want (nil, true)", v, ok)
	}
}

func TestOmittableOr(t *testing.T) {
	if got := (Omittable[string]{}).Or("def"); got != "def" {
		t.Fatalf("unset Or = %q", got)
	}
	if got := OmittableOf("set").Or("def"); got != "set" {
		t.Fatalf("set Or = %q", got)
	}
}

func TestOmittableMarshalJSON(t *testing.T) {
	var unset Omittable[string]
	b, err := json.Marshal(unset)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("unset marshals to %s, want null", b)
	}

	b, err = json.Marshal(OmittableOf("v"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"v"` {
		t.Fatalf("set marshals to %s, want \"v\"", b)
	}
}
