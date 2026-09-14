package graphql

import (
	"strings"
	"testing"

	"github.com/syssam/graphql-go/internal/jsonw"
)

type role int

const (
	roleAdmin role = iota
	roleUser
)

var roleNames = map[role]string{roleAdmin: "ADMIN", roleUser: "USER"}

func TestEnumRoundTrip(t *testing.T) {
	s, err := NewSchema(SDL(`enum Role { ADMIN USER } type Query { r: Role! rs: [Role] }`),
		Enum("Role", roleNames),
		Object[Root]("Query",
			Field("r", func(Root) role { return roleUser }),
			Field("rs", func(Root) []*role { return []*role{ptr(roleAdmin), nil} }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	w := jsonw.New()
	if err := s.query.fields["r"].writeLeaf(ctxBackground(), w, &Root{}, nil); err != nil || string(w.Bytes()) != `"USER"` {
		t.Fatalf("r: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := s.query.fields["rs"].writeLeaf(ctxBackground(), w, &Root{}, nil); err != nil || string(w.Bytes()) != `["ADMIN",null]` {
		t.Fatalf("rs: %v %s", err, w.Bytes())
	}

	dec := decoder[role](t, s.reg, "Role")
	if v, err := dec("ADMIN", parseType("Role!")); err != nil || v != roleAdmin {
		t.Fatalf("decode ADMIN = %v, %v", v, err)
	}
	if _, err := dec("NOPE", parseType("Role!")); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("unknown enum value should fail, got %v", err)
	}
	if _, err := dec(1, parseType("Role!")); err == nil {
		t.Fatal("non-string enum input should fail")
	}
}

func TestEnumUnknownGoValueOutput(t *testing.T) {
	s, err := NewSchema(SDL(`enum Role { ADMIN USER } type Query { r: Role! }`),
		Enum("Role", roleNames),
		Object[Root]("Query", Field("r", func(Root) role { return role(99) })),
	)
	if err != nil {
		t.Fatal(err)
	}
	w := jsonw.New()
	if err := s.query.fields["r"].writeLeaf(ctxBackground(), w, &Root{}, nil); err == nil {
		t.Fatal("unmapped Go value must fail on output")
	}
}

func TestEnumMappingValidation(t *testing.T) {
	base := func(opts ...SchemaOption) error {
		return Validate(SDL(`enum Role { ADMIN USER GUEST } type Query { r: Role! }`),
			append(opts, Object[Root]("Query", Field("r", func(Root) role { return roleUser })))...)
	}
	err := base(Enum("Role", roleNames))
	if err == nil || !strings.Contains(err.Error(), "GUEST") {
		t.Fatalf("missing GUEST mapping should fail, got %v", err)
	}
	err = base(Enum("Role", map[role]string{roleAdmin: "ADMIN", roleUser: "USER", 2: "GUEST", 3: "NOPE"}))
	if err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("unknown SDL value should fail, got %v", err)
	}
	err = base(Enum("Role", map[role]string{roleAdmin: "ADMIN", roleUser: "ADMIN", 2: "GUEST"}))
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("duplicate mapping should fail, got %v", err)
	}
}
