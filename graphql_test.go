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

func TestOmittableUnmarshalJSON(t *testing.T) {
	type input struct {
		Name  Omittable[*string]  `json:"name"`
		Count Omittable[int]      `json:"count"`
		Tags  Omittable[[]string] `json:"tags"`
	}

	t.Run("a value is decoded and marked set", func(t *testing.T) {
		var in input
		if err := json.Unmarshal([]byte(`{"name":"a","count":3,"tags":["x","y"]}`), &in); err != nil {
			t.Fatal(err)
		}
		name, ok := in.Name.ValueOK()
		if !ok || name == nil || *name != "a" {
			t.Fatalf("name = %v, set %v", name, ok)
		}
		if got, ok := in.Count.ValueOK(); !ok || got != 3 {
			t.Fatalf("count = %v, set %v", got, ok)
		}
		if got := in.Tags.Value(); len(got) != 2 || got[1] != "y" {
			t.Fatalf("tags = %v", got)
		}
	})

	t.Run("an explicit null is set, with the zero value", func(t *testing.T) {
		var in input
		if err := json.Unmarshal([]byte(`{"name":null}`), &in); err != nil {
			t.Fatal(err)
		}
		if !in.Name.IsSet() || in.Name.Value() != nil {
			t.Fatalf("explicit null: set=%v value=%v", in.Name.IsSet(), in.Name.Value())
		}
	})

	t.Run("an absent field stays unset", func(t *testing.T) {
		var in input
		if err := json.Unmarshal([]byte(`{"count":1}`), &in); err != nil {
			t.Fatal(err)
		}
		if in.Name.IsSet() || in.Tags.IsSet() {
			t.Fatal("fields missing from the JSON must stay unset")
		}
		if !in.Count.IsSet() {
			t.Fatal("the present field must be set")
		}
	})

	t.Run("a value of the wrong type is an error and leaves the field unset", func(t *testing.T) {
		var in input
		if err := json.Unmarshal([]byte(`{"count":"three"}`), &in); err == nil {
			t.Fatal("decoding a string into Omittable[int] must fail")
		}
		if in.Count.IsSet() {
			t.Fatal("a failed decode must not mark the field set")
		}
	})

	t.Run("it round-trips a set value", func(t *testing.T) {
		b, err := json.Marshal(input{Name: OmittableOf(ptrTo("z")), Count: OmittableOf(7)})
		if err != nil {
			t.Fatal(err)
		}
		var back input
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if got, _ := back.Count.ValueOK(); got != 7 || !back.Name.IsSet() {
			t.Fatalf("round trip lost data: %s -> %+v", b, back)
		}
	})
}

func ptrTo[T any](v T) *T { return &v }
