package graphql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// authzInputSDL is named distinctly from input_test.go's inputSDL, which
// already exists in this package for the decode tests.
//
// JSON is a custom scalar that accepts a bare identifier literal (fix
// round 1, R3): the same lexical shape as an enum value, so it is the fixture
// for proving a custom scalar is never mistaken for one. Where.and takes
// nullable elements so a literal or variable list can carry an explicit null
// element (R5a) without failing validation. Choice is a oneOf input (item 4);
// gqlparser enforces its "exactly one key" rule itself, and the walk needs no
// special case for it because it never looks at directives.
const authzInputSDL = `
scalar JSON

directive @oneOf on INPUT_OBJECT

enum OrderField { NAME TAX_NUMBER }
enum Direction { ASC DESC }
input Order { field: OrderField! direction: Direction = ASC }
input Where { not: Where and: [Where] nameContains: String taxNumber: String meta: JSON metaTags: [JSON] }
input Patch { name: String taxNumber: String }
input Choice @oneOf { byId: ID byName: String }
type Query {
  customers(where: Where, orderBy: [Order!], groupBy: [OrderField!], meta: JSON, tags: [String]): [String!]!
  lookup(choice: Choice): String
}
type Mutation { update(patch: Patch!): String }
`

// supplied parses a query and returns the argument's type, supplied value and
// the schema's types, so each case states the operation exactly as a client
// would send it.
func supplied(t *testing.T, query, field, arg string) (map[string]*ast.Definition, *ast.Type, *ast.Value) {
	t.Helper()
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: authzInputSDL})
	doc, errs := gqlparser.LoadQuery(schema, query)
	if errs != nil {
		t.Fatalf("query: %v", errs)
	}
	sel := doc.Operations[0].SelectionSet[0].(*ast.Field)
	if sel.Name != field {
		t.Fatalf("first selection is %s, want %s", sel.Name, field)
	}
	def := sel.Definition.Arguments.ForName(arg)
	var v *ast.Value
	if a := sel.Arguments.ForName(arg); a != nil {
		v = a.Value
	}
	return schema.Types, def.Type, v
}

func render(keys []InputKey) []string {
	var out []string
	for _, k := range keys {
		s := strings.Join(k.Path, ".")
		if k.Enum != "" {
			s += "=" + k.Enum
		}
		if k.Null {
			s += "=null"
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestInputKeys(t *testing.T) {
	cases := []struct {
		name  string
		query string
		field string
		arg   string
		vars  map[string]any
		want  []string
	}{
		{"filter keys, scalar values never reported",
			`{ customers(where: {nameContains: "ada", taxNumber: "123"}) }`, "customers", "where", nil,
			[]string{"nameContains", "taxNumber"}},
		{"nested and list, indices omitted, deduplicated",
			`{ customers(where: {and: [{taxNumber: "1"}, {taxNumber: "2"}], not: {nameContains: "x"}}) }`, "customers", "where", nil,
			[]string{"and", "and.taxNumber", "not", "not.nameContains"}},
		{"ordering by enum value",
			`{ customers(orderBy: [{field: TAX_NUMBER}]) }`, "customers", "orderBy", nil,
			[]string{"field=TAX_NUMBER"}},
		{"SDL input-field default is not reported",
			`{ customers(orderBy: [{field: NAME}]) }`, "customers", "orderBy", nil,
			[]string{"field=NAME"}},
		{"enum list argument reports each value",
			`{ customers(groupBy: [NAME, TAX_NUMBER]) }`, "customers", "groupBy", nil,
			[]string{"=NAME", "=TAX_NUMBER"}},
		{"variable object typed against the schema",
			`query($o: [Order!]) { customers(orderBy: $o) }`, "customers", "orderBy",
			map[string]any{"o": []any{map[string]any{"field": "TAX_NUMBER", "direction": "DESC"}}},
			[]string{"direction=DESC", "field=TAX_NUMBER"}},
		{"variable nested inside a literal",
			`query($f: OrderField!) { customers(orderBy: [{field: $f}]) }`, "customers", "orderBy",
			map[string]any{"f": "TAX_NUMBER"},
			[]string{"field=TAX_NUMBER"}},
		{"absent variable reports nothing",
			`query($w: Where) { customers(where: $w) }`, "customers", "where", map[string]any{},
			nil},
		{"explicit null key is reported",
			`mutation { update(patch: {taxNumber: null}) }`, "update", "patch", nil,
			[]string{"taxNumber=null"}},
		{"absent key is not a write",
			`mutation { update(patch: {name: "a"}) }`, "update", "patch", nil,
			[]string{"name"}},
		{"explicit null through a variable",
			`mutation($p: Patch!) { update(patch: $p) }`, "update", "patch",
			map[string]any{"p": map[string]any{"taxNumber": nil}},
			[]string{"taxNumber=null"}},
		{"argument not supplied",
			`{ customers }`, "customers", "where", nil,
			nil},

		// Fix round 1, R3: a custom scalar's bare-identifier literal is
		// lexically an ast.EnumValue but must never be reported as Enum.
		{"custom scalar literal at top level is not an enum",
			`{ customers(meta: SECRET) }`, "customers", "meta", nil,
			nil},
		{"custom scalar literal in an input field is a bare key",
			`{ customers(where: {meta: SECRET}) }`, "customers", "where", nil,
			[]string{"meta"}},
		{"custom scalar literal in a list is a bare key, deduplicated",
			`{ customers(where: {metaTags: [SECRET1, SECRET2]}) }`, "customers", "where", nil,
			[]string{"metaTags"}},

		// Fix round 1, R4: a literal key whose value is a variable resolving
		// to an object or list is itself reported, exactly as for a literal
		// composite value at that key.
		{"literal key holding an empty variable object is still reported",
			`query($w: Where) { customers(where: {not: $w}) }`, "customers", "where",
			map[string]any{"w": map[string]any{}},
			[]string{"not"}},
		{"literal key holding an empty variable list is still reported",
			`query($a: [Where]) { customers(where: {and: $a}) }`, "customers", "where",
			map[string]any{"a": []any{}},
			[]string{"and"}},
		{"literal key holding a non-empty variable object reports the key and its contents",
			`query($w: Where) { customers(where: {not: $w}) }`, "customers", "where",
			map[string]any{"w": map[string]any{"nameContains": "x"}},
			[]string{"not", "not.nameContains"}},

		// Fix round 1, R5a: a null list element is not a null at the list's
		// key; the key is still reported as an intermediate key when nested.
		{"null element in a nested literal list is not a null at that key",
			`{ customers(where: {and: [null]}) }`, "customers", "where", nil,
			[]string{"and"}},
		{"null element in a top-level literal list reports nothing",
			`{ customers(tags: [null]) }`, "customers", "tags", nil,
			nil},
		{"null element in a nested variable list is not a null at that key",
			`query($a: [Where]) { customers(where: {and: $a}) }`, "customers", "where",
			map[string]any{"a": []any{nil}},
			[]string{"and"}},
		{"null element in a top-level variable list reports nothing",
			`query($t: [String]) { customers(tags: $t) }`, "customers", "tags",
			map[string]any{"t": []any{nil}},
			nil},

		// Fix round 1, R5c: an enum value a variable supplies that is not
		// declared on the enum is not reported at all; coercion would have
		// rejected it.
		{"an enum value not declared on the enum is not reported",
			`query($f: OrderField!) { customers(orderBy: [{field: $f}]) }`, "customers", "orderBy",
			map[string]any{"f": "BOGUS"},
			nil},

		// Item 4 follow-ups.
		{"argument itself sent as an explicit null",
			`{ customers(where: null) }`, "customers", "where", nil,
			[]string{"=null"}},
		{"argument sent as a variable resolved to null",
			`query($w: Where) { customers(where: $w) }`, "customers", "where",
			map[string]any{"w": nil},
			[]string{"=null"}},
		{"a single value stands in for a list through a variable",
			`query($o: [Order!]) { customers(orderBy: $o) }`, "customers", "orderBy",
			map[string]any{"o": map[string]any{"field": "TAX_NUMBER", "direction": "DESC"}},
			[]string{"direction=DESC", "field=TAX_NUMBER"}},
		{"a oneOf input reports whichever field was supplied",
			`{ lookup(choice: {byId: "1"}) }`, "lookup", "choice", nil,
			[]string{"byId"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			types, typ, v := supplied(t, tc.query, tc.field, tc.arg)
			var got []InputKey
			if v != nil {
				got = inputKeys(types, typ, v, tc.vars)
			}
			if r := render(got); !slices.Equal(r, tc.want) {
				t.Errorf("keys = %v, want %v", r, tc.want)
			}
		})
	}
}

// Fix round 1, R5b: the variable-object walk must visit map keys in sorted
// order so the reported slice is deterministic. This does not go through
// render(), which sorts its output and so cannot tell a fixed order from a
// lucky one; asserting the raw slice order is the point.
func TestInputKeysVariableMapOrderDeterministic(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	vars := map[string]any{"w": map[string]any{
		"taxNumber":    nil,
		"and":          nil,
		"not":          nil,
		"nameContains": nil,
	}}
	keys := inputKeys(types, typ, v, vars)
	var got []string
	for _, k := range keys {
		got = append(got, strings.Join(k.Path, "."))
	}
	want := []string{"and", "nameContains", "not", "taxNumber"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v (map iteration must be sorted)", got, want)
	}
}

// A variable's value never appears in a key: only paths and enum identifiers.
func TestInputKeysNeverCarryScalarValues(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	keys := inputKeys(types, typ, v, map[string]any{"w": map[string]any{"taxNumber": "SECRET-123"}})
	for _, k := range keys {
		if strings.Contains(strings.Join(k.Path, "."), "SECRET") || strings.Contains(k.Enum, "SECRET") {
			t.Fatalf("a scalar value leaked into %+v", k)
		}
	}
}

const inputDirectiveSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION | FIELD_DEFINITION | INPUT_FIELD_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
enum OrderField { NAME TAX_NUMBER }
input Where { nameContains: String }
`

func TestAuthorizeInputPlacementAtBuild(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr string // empty means the schema must build
	}{
		{"input object argument", `type Query { c(where: Where @authorizeInput(kind: FILTER)): String }`, ""},
		{"enum list argument", `type Query { c(groupBy: [OrderField!] @authorizeInput(kind: FILTER)): String }`, ""},
		{"scalar argument", `type Query { c(q: String @authorizeInput(kind: FILTER)): String }`, "Query.c(q:)"},
		{"on a field", `type Query { c: String @authorizeInput(kind: FILTER) }`, "Query.c"},
		{"on an input field", `input Bad { a: Where @authorizeInput(kind: FILTER) } type Query { c(b: Bad): String }`, "Bad.a"},
		{"on an interface field argument", `interface Node { c(where: Where @authorizeInput(kind: FILTER)): String } type Query { c: String }`, "Node.c(where:)"},
		// Fix round 1, R6: gqlparser does not type-check a directive argument's
		// literal against its declared enum type, so these build cleanly
		// without this check even though Task 3 reads Raw == "WRITE" literally.
		{"kind an unknown enum name", `type Query { c(where: Where @authorizeInput(kind: BOGUS)): String }`, "Query.c(where:)"},
		{"kind a string literal", `type Query { c(where: Where @authorizeInput(kind: "FILTER")): String }`, "Query.c(where:)"},
		{"kind WRITE is valid", `type Query { c(where: Where @authorizeInput(kind: WRITE)): String }`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(inputDirectiveSDL+tc.extra), Query(Field("c", func(Root) *string { return nil })))
			if tc.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "authorizeInput") {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "authorizeInput") {
				t.Fatalf("want an @authorizeInput error naming %q, got %v", tc.wantErr, err)
			}
		})
	}

	// A repeatable declaration lets gqlparser accept a second occurrence on
	// the same argument; shapeBuilder.field then reads only ds.ForName's
	// first match (authz_shape.go), so without this rejection the second
	// occurrence would silently be dropped rather than reported and the
	// argument would build as whichever kind happened to come first.
	t.Run("repeated on one argument even when declared repeatable", func(t *testing.T) {
		const repeatableInputDirectiveSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) repeatable on ARGUMENT_DEFINITION | FIELD_DEFINITION | INPUT_FIELD_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Where { nameContains: String }
type Query { c(where: Where @authorizeInput(kind: FILTER) @authorizeInput(kind: WRITE)): String }
`
		_, err := NewSchema(SDL(repeatableInputDirectiveSDL), Query(Field("c", func(Root) *string { return nil })))
		want := "Query.c(where:)"
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "authorizeInput") {
			t.Fatalf("want an @authorizeInput error naming %q for a repeated occurrence, got %v", want, err)
		}
	})
}

const argSiteSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
enum OrderField { NAME TAX_NUMBER }
input Order { field: OrderField! }
input Where { nameContains: String taxNumber: String }
input Patch { name: String taxNumber: String }
interface Entity { name: String! }
type Customer implements Entity {
  name: String!
  related(where: Where @authorizeInput(kind: FILTER)): [Customer!]!
}
type Query {
  customers(where: Where @authorizeInput(kind: FILTER), orderBy: [Order!] @authorizeInput(kind: FILTER)): [Customer!]!
  guarded(where: Where @authorizeInput(kind: FILTER)): [Customer!]! @requiresScopes(scopes: [["c:read"]])
  plain: String
  entity: Entity
  pureFiltered(where: Where @authorizeInput(kind: FILTER)): String
}
type Mutation { update(patch: Patch! @authorizeInput(kind: WRITE)): String }
`

type argCustomer struct{ Name string }

// argWhereIn, argOrderIn and argPatchIn bind Where, Order and Patch. The
// brief's fixture used map[string]any fields directly, but NewSchema decodes
// an input object argument through a registered Input[T] struct binding, not
// a bare map -- there is no decoder keyed by map[string]any. These structs
// exist only so the schema builds; the tests below never inspect their
// fields.
type argWhereIn struct {
	NameContains *string
	TaxNumber    *string
}
type argOrderIn struct {
	Field string
}
type argPatchIn struct {
	Name      *string
	TaxNumber *string
}

// argCustomersArgs covers Query.customers; argGuardedArgs covers
// Query.guarded, which takes only where; argUpdateArgs covers
// Mutation.update. inputBinding.build requires every field an Args[...]
// registration declares to exist on the site it is used for, so one struct
// cannot span customers (where, orderBy), guarded (where only) and update
// (patch) the way the brief's single argArgs sketched -- guarded has no
// orderBy argument and update has no where or orderBy.
type argCustomersArgs struct {
	Where   *argWhereIn
	OrderBy []argOrderIn
}
type argGuardedArgs struct {
	Where *argWhereIn
}
type argUpdateArgs struct {
	Patch argPatchIn
}

// argRelatedArgs covers Customer.related, an argument site reached only
// through an interface parent (Query.entity: Entity, resolved dynamically to
// Customer) rather than directly off a root field. argPureArgs covers
// Query.pureFiltered, bound with FieldArgs rather than ResolveArgs so an
// argument site on a pure field is exercised too.
type argRelatedArgs struct {
	Where *argWhereIn
}
type argPureArgs struct {
	Where *argWhereIn
}

var argResolverCalls atomic.Int64

func argSiteSchema(t testing.TB) *Schema {
	t.Helper()
	customers := func(context.Context, Root, argCustomersArgs) ([]*argCustomer, error) {
		argResolverCalls.Add(1)
		return []*argCustomer{{Name: "ada"}}, nil
	}
	guarded := func(context.Context, Root, argGuardedArgs) ([]*argCustomer, error) {
		argResolverCalls.Add(1)
		return []*argCustomer{{Name: "ada"}}, nil
	}
	s, err := NewSchema(SDL(argSiteSDL),
		Enum[string]("OrderField", map[string]string{"NAME": "NAME", "TAX_NUMBER": "TAX_NUMBER"}),
		Input[argWhereIn]("Where",
			InputField("nameContains", func(w *argWhereIn, v *string) { w.NameContains = v }),
			InputField("taxNumber", func(w *argWhereIn, v *string) { w.TaxNumber = v }),
		),
		Input[argOrderIn]("Order",
			InputField("field", func(o *argOrderIn, v string) { o.Field = v }),
		),
		Input[argPatchIn]("Patch",
			InputField("name", func(p *argPatchIn, v *string) { p.Name = v }),
			InputField("taxNumber", func(p *argPatchIn, v *string) { p.TaxNumber = v }),
		),
		Args[argCustomersArgs](
			InputField("where", func(a *argCustomersArgs, v *argWhereIn) { a.Where = v }),
			InputField("orderBy", func(a *argCustomersArgs, v []argOrderIn) { a.OrderBy = v }),
		),
		Args[argGuardedArgs](InputField("where", func(a *argGuardedArgs, v *argWhereIn) { a.Where = v })),
		Args[argUpdateArgs](InputField("patch", func(a *argUpdateArgs, v argPatchIn) { a.Patch = v })),
		Args[argRelatedArgs](InputField("where", func(a *argRelatedArgs, v *argWhereIn) { a.Where = v })),
		Args[argPureArgs](InputField("where", func(a *argPureArgs, v *argWhereIn) { a.Where = v })),
		// Entity is left unbound: like fixtureSDL's Node, a single implementing
		// type resolves from the dynamic Go type alone (objectForGoType).
		Query(
			ResolveArgs("customers", customers),
			ResolveArgs("guarded", guarded),
			Field("plain", func(Root) *string { return nil }),
			Field("entity", func(Root) *argCustomer { return &argCustomer{Name: "ada"} }),
			FieldArgs("pureFiltered", func(Root, argPureArgs) *string {
				argResolverCalls.Add(1)
				s := "ok"
				return &s
			}),
		),
		Mutation(ResolveArgs("update", func(context.Context, Root, argUpdateArgs) (*string, error) {
			argResolverCalls.Add(1)
			return nil, nil
		})),
		Object[argCustomer]("Customer",
			Field("name", func(c *argCustomer) string { return c.Name }),
			ResolveArgs("related", func(_ context.Context, _ *argCustomer, _ argRelatedArgs) ([]*argCustomer, error) {
				argResolverCalls.Add(1)
				return []*argCustomer{{Name: "related"}}, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

func TestArgumentSitesAtPlanCompile(t *testing.T) {
	e := NewExecutor(argSiteSchema(t))
	// where is supplied through a variable and orderBy is left absent, so the
	// two argument sites exercise both halves of D3's "supplied means sent by
	// the client": one site's argValue must come back nil, the other must
	// carry the variable reference itself, not a resolved value.
	p, _, perrs := planForTest(t, e, `query($w: Where) { customers(where: $w) { name } plain }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	if !p.shape.hasArgSites {
		t.Fatal("shape does not record argument sites")
	}
	sites := p.shape.Sites()
	var coords []string
	byArg := map[string]AuthSite{}
	for _, s := range sites {
		if s.Kind == SiteFilterArg {
			coords = append(coords, s.Coord)
			if s.Arg == "" {
				t.Errorf("site %s has no Arg", s.Coord)
			}
			byArg[s.Arg] = s
		}
	}
	slices.Sort(coords)
	if want := []string{"Query.customers(orderBy:)", "Query.customers(where:)"}; !slices.Equal(coords, want) {
		t.Errorf("argument sites = %v, want %v (a site exists even when the argument is not supplied)", coords, want)
	}

	where, ok := byArg["where"]
	if !ok {
		t.Fatal("no argument site for where")
	}
	if where.Arg != "where" {
		t.Errorf("where site Arg = %q, want %q exactly", where.Arg, "where")
	}
	if where.argValue == nil || where.argValue.Kind != ast.Variable {
		t.Errorf("where argValue = %+v, want the $w variable reference", where.argValue)
	}

	orderBy, ok := byArg["orderBy"]
	if !ok {
		t.Fatal("no argument site for orderBy")
	}
	if orderBy.argValue != nil {
		t.Errorf("orderBy argValue = %+v, want nil: the client did not supply it", orderBy.argValue)
	}
	if got := orderBy.argType.String(); got != "[Order!]" {
		t.Errorf("orderBy argType = %q, want [Order!]", got)
	}

	sel := p.sel.forType(p.root)
	for _, f := range sel.fields {
		switch f.name {
		case "customers":
			if f.authIdx < 0 {
				t.Error("a field with argument sites must carry an output site to route into enforceAuth")
			}
			if f.argSiteCount() != 2 {
				t.Errorf("customers argSites = %d, want 2", f.argSiteCount())
			}
		case "plain":
			if f.authIdx != -1 || f.argSites != 0 {
				t.Errorf("plain field gained authorization state: authIdx=%d argSites=%d", f.authIdx, f.argSites)
			}
		}
	}
}

// TestArgumentSiteWriteKind proves the WRITE half of the kind switch: without
// it every @authorizeInput site would classify as SiteFilterArg regardless of
// its declared kind.
func TestArgumentSiteWriteKind(t *testing.T) {
	e := NewExecutor(argSiteSchema(t))
	p, _, perrs := planForTest(t, e, `mutation { update(patch: {name: "a"}) }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	var found *AuthSite
	for _, s := range p.shape.Sites() {
		if s.Arg == "patch" {
			s := s
			found = &s
		}
	}
	if found == nil {
		t.Fatal("no argument site for patch")
	}
	if found.Kind != SiteInputWrite {
		t.Errorf("patch site kind = %v, want SiteInputWrite", found.Kind)
	}
	if want := "Mutation.update(patch:)"; found.Coord != want {
		t.Errorf("patch site coord = %q, want %q", found.Coord, want)
	}
}

// TestArgumentSiteRoutingPreservesFieldRequirement guards the exact bug a
// naive `len(argSites) > 0` routing condition reintroduces: guarded already
// carries its own @requiresScopes, and the synthetic zero-Requires output
// site built for a field with no requirement of its own must never replace
// it.
func TestArgumentSiteRoutingPreservesFieldRequirement(t *testing.T) {
	s := argSiteSchema(t)
	e := NewExecutor(s)
	p, _, perrs := planForTest(t, e, `{ guarded { name } }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	sel := p.sel.forType(p.root)
	var f *planField
	for _, ff := range sel.fields {
		if ff.name == "guarded" {
			f = ff
		}
	}
	if f == nil {
		t.Fatal("guarded not selected")
	}
	if f.authIdx < 0 {
		t.Fatal("guarded has no output site")
	}
	out := p.shape.sites[f.authIdx]
	if out.Kind != SiteOutput {
		t.Errorf("guarded's own site kind = %v, want SiteOutput", out.Kind)
	}
	if got := out.Requires.Scopes(); !slices.Equal(got, []string{"c:read"}) {
		t.Errorf("guarded's own requirement = %v, want [c:read]: routing overwrote it with a zero-Requires site", got)
	}
	if f.argSiteCount() != 1 {
		t.Fatalf("guarded argSites = %d, want 1", f.argSiteCount())
	}
	arg := p.shape.sites[f.authIdx+1]
	if arg.Kind != SiteFilterArg || arg.Arg != "where" {
		t.Errorf("guarded argument site = %+v, want a SiteFilterArg for where", arg)
	}

	authz := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(func(context.Context) map[string]bool { return nil })))
	resp := run(t, authz, `{ guarded { name } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("guarded was not rejected for a caller holding no scopes: its requirement was lost")
	}
	if got, want := resp.Errors[0].Extensions["code"], CodeForbidden; got != want {
		t.Errorf("code = %v, want %v", got, want)
	}
}

func TestPlanWithoutArgumentSitesIsUnchanged(t *testing.T) {
	e := NewExecutor(argSiteSchema(t))
	p, _, perrs := planForTest(t, e, `{ plain }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	if !p.shape.IsEmpty() {
		t.Errorf("a plan touching no guarded field or argument built sites: %v", p.shape.Sites())
	}
}

// taxPolicy denies any filter or ordering that touches taxNumber, and any write
// that sets it -- the consumer's field control in miniature.
func taxPolicy(seen *[]InputKey) Authorizer {
	return AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
		for i, s := range shape.Sites() {
			if s.Kind != SiteFilterArg && s.Kind != SiteInputWrite {
				continue
			}
			for _, k := range d.Input(i) {
				if seen != nil {
					*seen = append(*seen, k)
				}
				if slices.Contains(k.Path, "taxNumber") || k.Enum == "TAX_NUMBER" {
					if err := d.Set(i, Deny("customer:taxNumber", s.Coord)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func TestArgumentSiteDenyRefusesTheField(t *testing.T) {
	cases := []struct {
		name  string
		query string
		vars  string
		deny  bool
	}{
		{"filtering by a restricted key", `{ customers(where: {taxNumber: "1"}) { name } }`, "", true},
		{"ordering by a restricted enum value", `{ customers(orderBy: [{field: TAX_NUMBER}]) { name } }`, "", true},
		{"ordering via a variable", `query($o: [Order!]) { customers(orderBy: $o) { name } }`, `{"o":[{"field":"TAX_NUMBER"}]}`, true},
		{"writing a restricted field to null", `mutation { update(patch: {taxNumber: null}) }`, "", true},
		{"an unrestricted filter runs", `{ customers(where: {nameContains: "a"}) { name } }`, "", false},
		{"an unrestricted write runs", `mutation { update(patch: {name: "a"}) }`, "", false},

		// An argument site reached only through an interface parent: Query.entity
		// is typed Entity, and the site lives on the concrete Customer.related
		// field selected under an inline fragment.
		{"through an interface parent, restricted", `{ entity { ... on Customer { related(where: {taxNumber: "1"}) { name } } } }`, "", true},
		{"through an interface parent, unrestricted", `{ entity { ... on Customer { related(where: {nameContains: "a"}) { name } } } }`, "", false},

		// A named fragment selected under @include with an alias on the field
		// carrying the argument site.
		{"through a named fragment, alias and @include, restricted",
			`query($i: Boolean!) { picked: customers(where: {taxNumber: "1"}) @include(if: $i) { ...CustomerFields } } fragment CustomerFields on Customer { name }`,
			`{"i":true}`, true},
		{"through a named fragment, alias and @include, unrestricted",
			`query($i: Boolean!) { picked: customers(where: {nameContains: "a"}) @include(if: $i) { ...CustomerFields } } fragment CustomerFields on Customer { name }`,
			`{"i":true}`, false},

		// A pure FieldArgs field (no context, no error) still routes through
		// enforceAuth before it runs.
		{"through a pure FieldArgs field, restricted", `{ pureFiltered(where: {taxNumber: "1"}) }`, "", true},
		{"through a pure FieldArgs field, unrestricted", `{ pureFiltered(where: {nameContains: "a"}) }`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argResolverCalls.Store(0)
			e := NewExecutor(argSiteSchema(t), WithAuthorizer(taxPolicy(nil)))
			resp := run(t, e, tc.query, tc.vars)
			if !tc.deny {
				if len(resp.Errors) > 0 {
					t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
				}
				if argResolverCalls.Load() != 1 {
					t.Errorf("resolver ran %d times, want 1", argResolverCalls.Load())
				}
				return
			}
			if len(resp.Errors) != 1 {
				t.Fatalf("errors = %s, want exactly one denial", errorsJSON(resp.Errors))
			}
			if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
				t.Errorf("code = %v, want %v", got, CodeForbidden)
			}
			if n := argResolverCalls.Load(); n != 0 {
				t.Errorf("resolver ran %d times; a denied argument must refuse before it runs", n)
			}
		})
	}
}

func TestArgumentSiteInputIsReportedOnlyWhenPlanHasArgumentSites(t *testing.T) {
	var seen []InputKey
	e := NewExecutor(argSiteSchema(t), WithAuthorizer(taxPolicy(&seen)))
	run(t, e, `{ customers(where: {nameContains: "SECRET"}, orderBy: [{field: NAME}]) { name } }`, "")
	got := render(seen)
	if !slices.Contains(got, "nameContains") || !slices.Contains(got, "field=NAME") {
		t.Errorf("reported inputs = %v", got)
	}
	for _, k := range got {
		if strings.Contains(k, "SECRET") {
			t.Errorf("a scalar value reached the Authorizer: %v", got)
		}
	}
}

func TestArgumentSiteAdmitsOnlyAllowAndDeny(t *testing.T) {
	for _, o := range []struct {
		name    string
		out     Outcome
		wantErr bool
	}{
		{"Allow", Allow(), false},
		{"Deny", Deny("p", "r"), false},
		{"Null", Null(), true},
		{"Zero", Zero(), true},
		{"Redact", Redact(func(v any) any { return v }), true},
	} {
		t.Run(o.name, func(t *testing.T) {
			var setErr error
			e := NewExecutor(argSiteSchema(t), WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					for i, s := range shape.Sites() {
						if s.Kind == SiteFilterArg {
							setErr = d.Set(i, o.out)
							return nil
						}
					}
					return nil
				})))
			run(t, e, `{ customers(where: {nameContains: "a"}) { name } }`, "")
			if (setErr != nil) != o.wantErr {
				t.Errorf("Set(%s) error = %v, wantErr %v", o.name, setErr, o.wantErr)
			}
		})
	}
}

// A field that carries both an output requirement and an argument site must
// honour both: an allowed output with a denied argument is still refused.
func TestArgumentDenyAppliesAlongsideAnOutputRequirement(t *testing.T) {
	argResolverCalls.Store(0)
	e := NewExecutor(argSiteSchema(t), WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			if err := ScopeAuthorizer(func(context.Context) map[string]bool {
				return map[string]bool{"c:read": true}
			}).Authorize(ctx, shape, d); err != nil {
				return err
			}
			return taxPolicy(nil).Authorize(ctx, shape, d)
		})))
	resp := run(t, e, `{ guarded(where: {taxNumber: "1"}) { name } }`, "")
	if len(resp.Errors) == 0 || argResolverCalls.Load() != 0 {
		t.Fatalf("argument denial ignored when the output site allowed: errors=%s calls=%d", errorsJSON(resp.Errors), argResolverCalls.Load())
	}
}

// argSubSDL puts an argument site on a subscription root field that declares
// no requirement of its own, so the only thing that can refuse the stream is
// the argument.
const argSubSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Where { nameContains: String taxNumber: String }
type Message { id: ID! secret: String! }
type Query { ping: String! }
type Subscription { messages(where: Where @authorizeInput(kind: FILTER)): Message! }
`

type argSubArgs struct {
	Where *argWhereIn
}

// argSubOpenedWith records the where argument the last opened source received.
var argSubOpenedWith atomic.Pointer[argWhereIn]

func newArgSubExecutor(t *testing.T, a Authorizer, opts ...ExecutorOption) (*authSubSource, *Executor) {
	t.Helper()
	argSubOpenedWith.Store(nil)
	src := &authSubSource{ch: make(chan *authSubMessage)}
	s, err := NewSchema(SDL(argSubSDL),
		Query(Field("ping", func(Root) string { return "pong" })),
		Input[argWhereIn]("Where",
			InputField("nameContains", func(w *argWhereIn, v *string) { w.NameContains = v }),
			InputField("taxNumber", func(w *argWhereIn, v *string) { w.TaxNumber = v }),
		),
		Args[argSubArgs](InputField("where", func(a *argSubArgs, v *argWhereIn) { a.Where = v })),
		Object[authSubMessage]("Message",
			Field("id", func(m *authSubMessage) ID { return ID(m.ID) }),
			Field("secret", func(m *authSubMessage) string { return m.Secret }),
		),
		Subscription(
			SubscribeArgs("messages", func(_ context.Context, args argSubArgs) (<-chan *authSubMessage, error) {
				src.opens.Add(1)
				where := args.Where
				if where == nil {
					where = &argWhereIn{}
				}
				argSubOpenedWith.Store(where)
				return src.ch, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return src, NewExecutor(s, append([]ExecutorOption{WithAuthorizer(a)}, opts...)...)
}

// An interceptor may replace OperationContext.Variables before the handler
// runs. The Authorizer reads the replaced map, so the source must be opened
// with arguments decoded from that same map: otherwise an interceptor shows
// the policy an allowed filter and opens the stream with a denied one.
func TestSubscribeOpensWithTheInputTheAuthorizerSaw(t *testing.T) {
	cases := []struct {
		name      string
		sent      string
		rewritten map[string]any
		wantOpen  bool
	}{
		{"denied input rewritten to allowed", `{"w":{"taxNumber":"1"}}`,
			map[string]any{"w": map[string]any{"nameContains": "a"}}, true},
		{"allowed input rewritten to denied", `{"w":{"nameContains":"a"}}`,
			map[string]any{"w": map[string]any{"taxNumber": "1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []InputKey
			rewrite := SubscriptionInterceptorFunc(func(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error) {
				oc.Variables = tc.rewritten
				return next(ctx, oc)
			})
			src, e := newArgSubExecutor(t, taxPolicy(&seen), WithSubscriptionInterceptor(rewrite))
			_, err := e.Subscribe(t.Context(), &Request{
				Query:     `subscription($w: Where) { messages(where: $w) { id } }`,
				Variables: []byte(tc.sent),
			})
			if !tc.wantOpen {
				if err == nil || src.opens.Load() != 0 {
					t.Fatalf("rewritten denied input opened the source: err=%v opens=%d", err, src.opens.Load())
				}
				return
			}
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			where := argSubOpenedWith.Load()
			if where == nil {
				t.Fatal("source was not opened")
			}
			var opened []string
			if where.NameContains != nil {
				opened = append(opened, "nameContains")
			}
			if where.TaxNumber != nil {
				opened = append(opened, "taxNumber")
			}
			if saw := render(seen); !slices.Equal(opened, saw) {
				t.Errorf("source opened with %v, Authorizer saw %v", opened, saw)
			}
		})
	}
}

const argOutputSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Where { nameContains: String taxNumber: String }
type Query { search(where: Where @authorizeInput(kind: FILTER)): String }
`

type argSearchArgs struct {
	Where *argWhereIn
}

// An argument Deny must win over whatever the field's own output site says.
// Null and Zero never run the resolver, so an order that applied them first
// would answer with a quiet null or "" and no error, hiding that the input
// was refused; Redact would run the resolver with the refused input.
func TestArgumentDenyWinsOverOutputOutcome(t *testing.T) {
	var calls atomic.Int64
	s, err := NewSchema(SDL(argOutputSDL),
		Input[argWhereIn]("Where",
			InputField("nameContains", func(w *argWhereIn, v *string) { w.NameContains = v }),
			InputField("taxNumber", func(w *argWhereIn, v *string) { w.TaxNumber = v }),
		),
		Args[argSearchArgs](InputField("where", func(a *argSearchArgs, v *argWhereIn) { a.Where = v })),
		Query(ResolveArgs("search", func(context.Context, Root, argSearchArgs) (*string, error) {
			calls.Add(1)
			v := "secret"
			return &v, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	for _, tc := range []struct {
		name string
		out  Outcome
	}{
		{"Null", Null()},
		{"Zero", Zero()},
		{"Redact", Redact(func(any) any { return "redacted" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					for i, site := range shape.Sites() {
						o := tc.out
						if site.Kind == SiteFilterArg {
							o = Deny("customer:taxNumber", site.Coord)
						}
						if err := d.Set(i, o); err != nil {
							return err
						}
					}
					return nil
				})))
			resp := run(t, e, `{ search(where: {taxNumber: "1"}) }`, "")
			if len(resp.Errors) != 1 {
				t.Fatalf("errors = %s, data = %s; want exactly one denial", errorsJSON(resp.Errors), resp.Data)
			}
			if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
				t.Errorf("code = %v, want %v", got, CodeForbidden)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("resolver ran %d times; a denied argument must refuse before it runs", n)
			}
		})
	}
}

// The argument arrives through a variable, so this also proves the open
// handler hands the Authorizer the operation's variables.
func TestSubscribeArgumentDenyDoesNotOpenTheSource(t *testing.T) {
	src, e := newArgSubExecutor(t, taxPolicy(nil))
	_, err := e.Subscribe(t.Context(), &Request{
		Query:     `subscription($w: Where) { messages(where: $w) { id } }`,
		Variables: []byte(`{"w":{"taxNumber":"1"}}`),
	})
	if n := src.opens.Load(); n != 0 {
		t.Errorf("source opened %d times, want 0: a denied argument must never reach it", n)
	}
	var serr *SubscribeError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %v, want a *SubscribeError", err)
	}
	if len(serr.Response.Errors) != 1 {
		t.Fatalf("got %d errors, want 1: %s", len(serr.Response.Errors), errorsJSON(serr.Response.Errors))
	}
	got := serr.Response.Errors[0]
	if got.Extensions["code"] != CodeForbidden {
		t.Errorf("code = %v, want %v", got.Extensions["code"], CodeForbidden)
	}
	if got, want := got.Path.String(), (Path{{Key: "messages"}}).String(); got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
}

func TestSubscribeAllowedArgumentOpensTheSource(t *testing.T) {
	src, e := newArgSubExecutor(t, taxPolicy(nil))
	ch, err := e.Subscribe(t.Context(), &Request{
		Query:     `subscription($w: Where) { messages(where: $w) { id } }`,
		Variables: []byte(`{"w":{"nameContains":"a"}}`),
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if n := src.opens.Load(); n != 1 {
		t.Errorf("source opened %d times, want 1", n)
	}
	sendEvent(t, src.ch, &authSubMessage{ID: "1"})
	resp := nextResponse(t, ch)
	if len(resp.Errors) != 0 {
		t.Fatalf("event errored: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"messages":{"id":"1"}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// Each event re-authorizes, and runSubscriptionEvent substitutes the root
// field's executor on a copy of its planField. An argument Deny recorded for
// an event must still refuse that event, as a root output Deny does, or a
// policy revoked mid-stream would keep delivering what it now forbids.
func TestSubscriptionEventArgumentDenyRefusesTheEvent(t *testing.T) {
	var revoked atomic.Bool
	src, e := newArgSubExecutor(t, AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			if !revoked.Load() {
				return nil
			}
			return taxPolicy(nil).Authorize(ctx, shape, d)
		}))
	ch, err := e.Subscribe(t.Context(), &Request{
		Query:     `subscription($w: Where) { messages(where: $w) { id } }`,
		Variables: []byte(`{"w":{"taxNumber":"1"}}`),
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sendEvent(t, src.ch, &authSubMessage{ID: "1"})
	if resp := nextResponse(t, ch); len(resp.Errors) != 0 {
		t.Fatalf("first event errored: %s", errorsJSON(resp.Errors))
	}

	revoked.Store(true)
	sendEvent(t, src.ch, &authSubMessage{ID: "2"})
	resp := nextResponse(t, ch)
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %s, want exactly one denial after revocation", errorsJSON(resp.Errors))
	}
	if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
		t.Errorf("code = %v, want %v", got, CodeForbidden)
	}
	if got, want := resp.Errors[0].Path.String(), (Path{{Key: "messages"}}).String(); got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
	if resp.Data != nil && string(resp.Data) != "null" {
		t.Errorf("denied event returned data: %s", resp.Data)
	}

	revoked.Store(false)
	sendEvent(t, src.ch, &authSubMessage{ID: "3"})
	if resp := nextResponse(t, ch); len(resp.Errors) != 0 {
		t.Fatalf("stream did not recover once the policy allowed again: %s", errorsJSON(resp.Errors))
	}
}
