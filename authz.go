package graphql

import (
	"context"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// Requirement is an OR of ANDs: satisfied when every scope in any one group
// is held. The zero Requirement is satisfied by everyone, so a field that
// declares nothing is not accidentally locked.
//
// The shape mirrors Apollo's @requiresScopes(scopes: [[String!]!]!), which is
// the vocabulary most tooling already reads.
type Requirement struct {
	anyOf [][]string
}

// NewRequirement builds a Requirement from its groups.
// Each group is cloned to prevent external mutations from changing the requirement
// after construction, which is important for Requirement's use on the concurrent
// request path and its immutability contract everywhere else.
func NewRequirement(anyOf ...[]string) Requirement {
	cloned := make([][]string, len(anyOf))
	for i, group := range anyOf {
		cloned[i] = slices.Clone(group)
	}
	return Requirement{anyOf: cloned}
}

// IsZero reports whether the requirement admits everyone.
func (r Requirement) IsZero() bool { return len(r.anyOf) == 0 }

// Satisfied reports whether held covers any one of the groups.
func (r Requirement) Satisfied(held map[string]bool) bool {
	if r.IsZero() {
		return true
	}
	for _, group := range r.anyOf {
		ok := true
		for _, scope := range group {
			if !held[scope] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Scopes returns every scope the requirement names, sorted and deduplicated,
// so a caller can load them from a policy source in one round trip.
func (r Requirement) Scopes() []string {
	var out []string
	for _, group := range r.anyOf {
		out = append(out, group...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// maxRequirementGroups bounds an effective requirement after inheritance.
// AND-ing OR-of-AND requirements multiplies their group counts, so a field
// inheriting from an object and several interfaces can grow quickly; past this
// NewSchema fails, keeping a pathological combination a build error rather
// than a per-request cost.
const maxRequirementGroups = 64

// And returns the requirement satisfied only when both r and o are. It is the
// cross product of their groups, each group sorted and deduplicated. The zero
// Requirement admits everyone, so it is the identity.
func (r Requirement) And(o Requirement) Requirement {
	if r.IsZero() {
		return o
	}
	if o.IsZero() {
		return r
	}
	out := make([][]string, 0, len(r.anyOf)*len(o.anyOf))
	for _, a := range r.anyOf {
		for _, b := range o.anyOf {
			g := make([]string, 0, len(a)+len(b))
			g = append(g, a...)
			g = append(g, b...)
			slices.Sort(g)
			out = append(out, slices.Compact(g))
		}
	}
	return Requirement{anyOf: out}
}

func (r Requirement) groupCount() int { return len(r.anyOf) }

// describe renders the requirement for a client-facing denial. It keeps the
// OR-of-AND structure rather than listing Scopes(), because inheritance
// usually produces a single AND group and flattening that into "a or b"
// tells a client either scope would do.
func (r Requirement) describe() string {
	var b strings.Builder
	for i, group := range r.anyOf {
		if i > 0 {
			b.WriteString(" or ")
		}
		paren := len(r.anyOf) > 1 && len(group) > 1
		if paren {
			b.WriteByte('(')
		}
		b.WriteString(strings.Join(group, " and "))
		if paren {
			b.WriteByte(')')
		}
	}
	return b.String()
}

// SiteKind says what kind of position needs an authorization decision.
type SiteKind uint8

const (
	// SiteOutput is a selected output field. A SiteOutput site can carry a
	// zero Requires: buildAuthShape adds one for a field that has argument
	// sites but no requirement of its own, purely to route the field through
	// enforceAuth, so an Authorizer iterating output sites will see it.
	SiteOutput SiteKind = iota
	// SiteObject is a whole object type, checked once per instance rather
	// than once per field of it.
	SiteObject
	// SiteFilterArg is an argument marked @authorizeInput(kind: FILTER) on
	// an object field. A restricted field must not be filterable either, or
	// its value leaks by bisection without ever being selected. Only Allow
	// and Deny are valid outcomes for it.
	SiteFilterArg
	// SiteInputWrite is an argument marked @authorizeInput(kind: WRITE) on
	// an object field. Only Allow and Deny are valid outcomes for it.
	SiteInputWrite
	// SiteInstance is one resolved object of an @authorizeObject type, about
	// to be written at this field position. Unlike every other site it is
	// decided during execution by an ObjectAuthorizer, because the values do
	// not exist when a Decision is built.
	SiteInstance
)

// AuthSite is one position in a plan that may need a decision.
type AuthSite struct {
	Coord string

	// Field and Object describe the position. Neither names a field for the
	// two kinds that are about a type rather than a field position:
	// SiteObject leaves Field nil, and SiteInstance leaves both nil, since an
	// instance is decided by its type and the value alone and the same
	// resolved object can reach several field positions. Coord carries the
	// type name for both.
	Field  *ast.FieldDefinition
	Object *ast.Definition

	Kind     SiteKind
	Requires Requirement
	Grants   []string

	// Arg names the argument for SiteFilterArg and SiteInputWrite sites. For
	// those kinds, Coord is Type.field(arg:) and Decision.Input reports what
	// the client supplied there.
	Arg string

	// leaf records whether Field is a scalar or enum, precomputed at shape
	// build from fieldDef.leaf (object.go). AuthSite carries only the AST,
	// and a field's own ast.FieldDefinition cannot answer this on its own:
	// a custom scalar's name is syntactically indistinguishable from an
	// object type's without walking the schema's type registry, which is
	// exactly what fieldDef.leaf already did once, at schema build.
	leaf bool

	// argType and argValue are the argument's declared type and the value the
	// operation supplied (nil when it supplied none), read at decision time
	// against the request's variables.
	argType  *ast.Type
	argValue *ast.Value
}

// AuthShape is what an operation touches, independent of who is asking. It
// is computed once per compiled plan and cached with it, which is why the
// plan cache is not multiplied by the number of distinct policies.
type AuthShape struct {
	sites  []AuthSite
	scopes []string

	// hasArgSites gates the per-request input walk, so a plan without
	// argument sites pays nothing for it.
	hasArgSites bool

	// hasInstanceSites gates the per-wave instance check the same way, so a
	// plan that selects no @authorizeObject type never assembles a batch.
	hasInstanceSites bool

	// src is the input-free decisionSource every Decision over this shape
	// shares when there is no input to carry, so newDecision allocates no
	// source of its own.
	src decisionSource
}

// Sites returns the positions needing a decision, indexed by site index.
// planField.authIdx indexes into this slice.
//
// The shape is cached with its plan and shared by every request that reuses
// it, and Authorize hands it to caller-supplied code, so this returns an
// independent copy rather than the plan's own slice: a caller that mutates
// a returned AuthSite's Coord, Requires or Grants must not corrupt
// authorization for a later request. Field and Object remain shared
// *ast.FieldDefinition/*ast.Definition pointers into the parsed schema —
// cloning gqlparser's AST per call would be its own, larger cost, and
// nothing in this package ever mutates that AST after NewSchema returns, so
// a caller would have to go out of its way to reach for a mutation this
// package itself never performs.
func (s *AuthShape) Sites() []AuthSite {
	if s == nil {
		return nil
	}
	out := make([]AuthSite, len(s.sites))
	copy(out, s.sites)
	for i := range out {
		out[i].Grants = slices.Clone(out[i].Grants)
	}
	return out
}

// Scopes returns every scope named anywhere in the operation, sorted and
// deduplicated, so an Authorizer can load them in one round trip. Like
// Sites, this is a copy of the plan's own slice, for the same reason: the
// shape is cached with the plan and shared by every later request.
func (s *AuthShape) Scopes() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.scopes)
}

// IsEmpty reports whether the operation touches nothing that declares a
// requirement and selects no field with an @authorizeInput argument. An
// Authorizer is not consulted for such an operation.
func (s *AuthShape) IsEmpty() bool { return s == nil || len(s.sites) == 0 }

type action uint8

const (
	actionAllow action = iota
	actionDeny
	actionNull
	actionZero
	actionRedact
	actionDrop
)

// Outcome is what the executor does with a site. The zero Outcome allows, so
// a Decision an Authorizer leaves untouched changes nothing.
type Outcome struct {
	act        action
	redact     func(any) any
	permission string
	resource   string
}

// Allow resolves the field normally.
func Allow() Outcome { return Outcome{} }

// Deny refuses the field. The message follows AIP-211: it reveals neither the
// value nor whether the resource exists, because choosing between
// PERMISSION_DENIED and NOT_FOUND is itself an existence oracle.
func Deny(permission, resource string) Outcome {
	return Outcome{act: actionDeny, permission: permission, resource: resource}
}

// Null writes null without resolving the field.
func Null() Outcome { return Outcome{act: actionNull} }

// Zero writes the zero value of the field's type without resolving it. It is
// how a non-null field is withheld without null-bubbling its parent.
func Zero() Outcome { return Outcome{act: actionZero} }

// Redact resolves the field and rewrites the result.
func Redact(fn func(any) any) Outcome { return Outcome{act: actionRedact, redact: fn} }

// Drop omits the value from its enclosing list. It is valid only at a list
// element position; dropping renumbers the indices that follow, so a later
// denial is reported at the index the client actually sees.
func Drop() Outcome { return Outcome{act: actionDrop} }

func (o Outcome) denial() *Error {
	return Errorf("Permission %q denied on resource %q (or it might not exist).", o.permission, o.resource).
		WithCode(CodeForbidden)
}

// zeroWritable reports whether t has a zero value this package can write
// without reflection. A list's zero is the empty list. Among leaves only the
// built-in scalars have one: an enum's zero would have to be a member the
// schema may not define, and a custom scalar's is the author's to decide.
func zeroWritable(t *ast.Type) bool {
	if t.Elem != nil {
		return true
	}
	switch t.NamedType {
	case "String", "ID", "Int", "Float", "Boolean":
		return true
	}
	return false
}

// isLeafField reports whether site's field is a leaf (scalar or enum) as
// opposed to a list or a composite (object/interface/union) type. It reuses
// the leaf-ness fieldDef already computed at schema build (object.go) via
// the existing isLeaf(schema, type) helper, rather than re-deriving it here
// from the AST alone, which cannot distinguish a custom scalar's name from
// an object type's without the schema's type registry.
func isLeafField(site AuthSite) bool { return site.leaf }

// validFor rejects an outcome the site cannot represent, so a policy mistake
// surfaces once per request with the coordinate attached rather than as a
// null-bubbled parent at write time.
func (o Outcome) validFor(site AuthSite) error {
	// An object site guards __typename, a String! with no resolver: Null
	// would write a silent spec-violating null, and Zero and Redact have no
	// field to act on.
	if site.Kind == SiteObject && o.act != actionAllow && o.act != actionDeny {
		return Errorf("authorization: only Allow and Deny are valid for %s, an object site guarding __typename", site.Coord)
	}
	// An argument site decides whether the field may run with the input it
	// was given; there is no value of its own to null, zero or redact.
	if (site.Kind == SiteFilterArg || site.Kind == SiteInputWrite) && o.act != actionAllow && o.act != actionDeny {
		return Errorf("authorization: only Allow and Deny are valid for %s, an argument site", site.Coord)
	}
	// An instance site decides whether one object may be seen. Zero and
	// Redact act on a leaf's value and have nothing to act on here.
	if site.Kind == SiteInstance && o.act != actionAllow && o.act != actionDeny &&
		o.act != actionNull && o.act != actionDrop {
		return Errorf("authorization: only Allow, Null, Deny and Drop are valid for %s, an instance site", site.Coord)
	}
	switch o.act {
	case actionNull:
		// A literal null on a non-null field is not a value the schema
		// allows, and enforceAuth's actionNull case writes it and reports
		// success: writeField only bubbles when writeFieldValue reports
		// failure, so this would reach the client as a silent, spec-
		// violating null with no error attached -- not as a bubbled
		// parent. Reject it here so the policy author's real options
		// (Zero for a leaf, Deny for anything) are the ones the type
		// system can represent.
		if site.Field != nil && site.Field.Type.NonNull {
			return Errorf("authorization: Null is not valid for %s, which is non-null; use Zero or Deny", site.Coord)
		}
	case actionZero:
		if site.Field == nil {
			return Errorf("authorization: Zero is not valid for %s, which is an object site", site.Coord)
		}
		if !zeroWritable(site.Field.Type) {
			alt := "Deny or Null"
			if site.Field.Type.NonNull {
				// Null is itself invalid here (see the actionNull case
				// above), so don't suggest a second dead end.
				alt = "Deny"
			}
			return Errorf("authorization: Zero is not valid for %s: %s has no zero value this package can write; use %s", site.Coord, site.Field.Type.String(), alt)
		}
	case actionRedact:
		if site.Field == nil || site.Field.Type.Elem != nil || !isLeafField(site) {
			return Errorf("authorization: Redact is valid only on a leaf field, not %s", site.Coord)
		}
		if o.redact == nil {
			return Errorf("authorization: Redact for %s was constructed with a nil function", site.Coord)
		}
	}
	return nil
}

// Authorizer turns an operation's shape into a decision for one principal.
// It runs once per query or mutation, before any field resolves, and is not
// called at all when the operation touches nothing that declares a
// requirement and selects no field with an @authorizeInput argument. A
// subscription calls it once when the stream opens and again for every
// event, so a scope revoked mid-stream applies to the next event.
//
// A returned error rejects the whole operation, or for a subscription the
// opening of the stream or the one event being evaluated. The interface deliberately
// holds no policy of its own: it is the adapter to whatever decides, be that
// OPA, Cedar, OpenFGA, Casbin or a hand-written checker.
type Authorizer interface {
	Authorize(ctx context.Context, shape *AuthShape, d *Decision) error
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(ctx context.Context, shape *AuthShape, d *Decision) error

// Authorize implements Authorizer.
func (f AuthorizerFunc) Authorize(ctx context.Context, shape *AuthShape, d *Decision) error {
	return f(ctx, shape, d)
}

// WithAuthorizer sets the authorizer consulted once per operation.
func WithAuthorizer(a Authorizer) ExecutorOption {
	return func(e *Executor) { e.authorizer = a }
}

// ObjectCheck is one resolved object an ObjectAuthorizer decides on. Type is
// the concrete object type's name, which differs from Site.Coord when the
// field's declared type is an interface or a union.
type ObjectCheck struct {
	Site   AuthSite
	Type   string
	Object any
}

// ObjectAuthorizer decides whether a principal may see individual objects.
// It is called once per wave with every guarded value in it, so a policy
// backed by a remote decision point issues one batched call rather than one
// per row. The returned slice is positional and must have the same length as
// checks; any other length is an error, because a short slice would silently
// allow the rows it does not cover.
type ObjectAuthorizer interface {
	AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error)
}

// WithObjectAuthorizer enables instance-level authorization. Without it,
// @authorizeObject describes positions and nothing is enforced.
func WithObjectAuthorizer(a ObjectAuthorizer) ExecutorOption {
	return func(e *Executor) { e.objectAuthorizer = a }
}

// WithObjectAuthBatch bounds how many checks one AuthorizeObjects call
// carries; larger waves are split into sequential calls. The default of 50
// matches OpenFGA's BatchCheck default, which is the shape most remote
// policy backends are tuned for.
func WithObjectAuthBatch(n int) ExecutorOption {
	return func(e *Executor) {
		if n > 0 {
			e.objectAuthBatch = n
		}
	}
}

// Decision records the outcome for each site in a shape. It is passed to the
// Authorizer rather than returned by it so the framework owns the allocation
// and sizes it from the shape. The zero value of every entry allows.
type Decision struct {
	src      *decisionSource
	outcomes []Outcome
}

// decisionSource is what a Decision reads besides its own outcomes. It sits
// behind a pointer rather than inline so Decision stays in the 32-byte size
// class: every authorized request allocates one, and a plan with no argument
// site shares the source cached on its AuthShape instead of paying for an
// input table it never fills.
type decisionSource struct {
	shape *AuthShape

	// inputs holds, per site, what the client supplied for an argument site.
	// Nil when the plan has no argument sites.
	inputs [][]InputKey
}

// Input returns what the client supplied for an argument site: key paths,
// enum values and explicit nulls, never scalar values. It is empty for any
// other site and for an argument the client did not supply.
func (d *Decision) Input(site int) []InputKey {
	if d == nil || d.src == nil || site < 0 || site >= len(d.src.inputs) {
		return nil
	}
	return d.src.inputs[site]
}

func newDecision(shape *AuthShape) *Decision {
	return &Decision{src: &shape.src, outcomes: make([]Outcome, len(shape.sites))}
}

// Set records the outcome for one site. It reports an error for an outcome
// the site cannot represent, so a policy mistake surfaces once per request
// with the coordinate attached rather than as a null-bubbled parent.
func (d *Decision) Set(site int, o Outcome) error {
	if d == nil || site < 0 || site >= len(d.outcomes) {
		return Errorf("authorization: site %d is out of range", site)
	}
	if err := o.validFor(d.src.shape.sites[site]); err != nil {
		return err
	}
	d.outcomes[site] = o
	return nil
}

// Outcome returns the recorded outcome for a site.
func (d *Decision) Outcome(site int) Outcome {
	if d == nil || site < 0 || site >= len(d.outcomes) {
		return Outcome{}
	}
	return d.outcomes[site]
}

// ScopeAuthorizer is the default policy: a site is allowed when held covers
// its Requirement, and denied with the AIP-211 wording otherwise. It exists
// so the common case needs no Authorizer of its own.
func ScopeAuthorizer(held func(context.Context) map[string]bool) Authorizer {
	return AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
		have := held(ctx)
		// shape.sites directly, not shape.Sites(): this is in-package,
		// read-only iteration, so it has no reason to pay for the defensive
		// copy Sites() makes for caller-supplied code.
		for i, site := range shape.sites {
			if site.Requires.Satisfied(have) {
				continue
			}
			if err := d.Set(i, Deny(site.Requires.describe(), site.Coord)); err != nil {
				return err
			}
		}
		return nil
	})
}
