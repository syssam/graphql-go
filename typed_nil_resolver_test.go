package graphql

import "testing"

type nilPet interface{ petKind() string }

type nilDog struct{ Name string }

func (d *nilDog) petKind() string { return "Dog" + d.Name[:0] }

// A typed nil at an abstract position is null, and was written as null when
// the Go type decided the object type. With a TypeResolver the resolver was
// called first, on the nil, and the usual resolver -- a method call on the
// value -- dereferenced it.
func TestATypedNilIsNullBeforeTheTypeResolverSeesIt(t *testing.T) {
	s, err := NewSchema(SDL(`
		interface Pet { name: String! }
		type Dog implements Pet { name: String! }
		type Query { pet: Pet pets: [Pet]! }
	`),
		Interface[nilPet]("Pet", TypeResolver(func(p nilPet) string { return p.petKind() })),
		Object[nilDog]("Dog", Field("name", func(d *nilDog) string { return d.Name })),
		Query(
			Field("pet", func(Root) nilPet { return (*nilDog)(nil) }),
			Field("pets", func(Root) []nilPet { return []nilPet{&nilDog{Name: "a"}, (*nilDog)(nil)} }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ pet { name } pets { name } }`, ""),
		`{"pet":null,"pets":[{"name":"a"},null]}`)
}
