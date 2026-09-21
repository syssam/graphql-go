# Pluggable requirement directives

- **Module:** `github.com/syssam/graphql-go`
- **Status:** design approved, not implemented
- **Files:** `authz.go`, `authz_shape.go`, `schema.go`
- **Follows:** `2026-09-16-authorization-design.md` §8, which this corrects

## 1. Problem

§8 of the authorization design states:

> P1 accepts any directive that yields a `Requirement`, so the core package is
> indifferent.

It is not. `authz_shape.go` declares `const authDirective = "requiresScopes"`,
`requirementOf` is unexported and reads only that name and only its `scopes`
argument, and no option anywhere supplies a different mapping. `NewRequirement`
is exported but nothing at schema build consumes a caller-supplied value.

The consequence is the migration §8 says the consumer should not be forced
through: 1,589 `@auth(requires: [String!])` sites cannot be enforced without
renaming every one of them. The spec has described an intent the code does not
have since P1 merged.

## 2. Goal

Let a schema declare which SDL directives the engine reads into a
`Requirement`, without weakening any check that applies to `@requiresScopes`
today.

### Non-goals

- A policy language. Unchanged from the parent design.
- `@authenticated` and `@policy`. They need shapes this design does not add
  (a directive with no argument, and a second namespace that is not scopes).
  They belong to the `ext/authz` follow-on, which is deliberately not specified
  here — see §7.
- Any change to the request path.

## 3. Surface

```go
// ScopeShape says how a directive's argument composes into a Requirement.
type ScopeShape uint8

const (
    ScopesNested ScopeShape = iota + 1 // [[String!]!] -- OR of ANDs
    ScopesAllOf                        // [String!]    -- one AND group
    ScopesAnyOf                        // [String!]    -- OR of single-scope groups
)

// RequirementDirective declares an additional SDL directive read into a
// Requirement.
func RequirementDirective(name, arg string, shape ScopeShape) SchemaOption
```

**`ScopeShape` has no valid zero value, and that is the point.** A flat
`[String!]` read as AND when the author meant OR silently widens access, and
the engine cannot recover the author's intent from the SDL: `@auth(requires:
["a","b"])` is the same text either way. An undeclared shape is a build error,
not a default.

**The option adds; it does not replace.** `@requiresScopes` keeps working, so a
schema can carry a legacy spelling for existing coordinates and the Apollo one
for new work. A consumer that uses only `@auth` is unaffected by the default
staying active, because nothing in its SDL matches it.

**A name may be declared once.** Declaring the same directive twice, including
redeclaring `requiresScopes` with a different shape, is a build error rather
than a last-wins race between two option calls.

**A declared directive must be usable.** If the SDL declares the named
directive but it has no argument by the given name, that is a build error. The
alternative is the quietest failure available: every site reads as
"no requirement" and the schema builds clean.

**Two declarations on one coordinate AND together**, which is the rule
`requirementOf` already applies to repeated occurrences of one directive. The
composition rule for authorization is that more declarations never mean less
restrictive.

## 4. What changes

The spelling is welded into four places. The correctness of this design is that
they move together; moving only the first is the fail-open in §5.

| Today | Becomes |
|---|---|
| `const authDirective` | a slice of shapes on `schemaBuilder`, defaulted to the Apollo one |
| `requirementOf(ds)` | a `schemaBuilder` method iterating the configured shapes |
| `checkRequiresScopes` | a per-shape literal check: nested wants `[[String!]!]`, flat wants `[String!]` |
| `rejectUnenforced` | rejects any configured name, not one |
| errors naming `@requiresScopes` | name the directive that actually appeared |

`resolveAuthRequirements` keeps its position as the one place a requirement is
computed. `maxRequirementGroups`, the interface-combination logic and the cap
are read from there unchanged; this design widens what feeds them, not who owns
them.

## 5. The failure this exists to prevent

If a custom directive is *read* but the placement check still looks only for
`@requiresScopes`, then `@auth` on a schema definition, a directive argument or
an input field is silently ignored. The author has written an authorization
requirement that does nothing, and no build error says so.

That is the same class as the defects the parent design's P5 and the
`validateObjectDirectives` work closed, and it is the reason approach B -- a
caller-supplied `func(ast.DirectiveList) (Requirement, bool)` -- was rejected:
it hands the engine a requirement without telling it a name, so the engine
cannot reject a misplacement it can no longer recognise. CLAUDE.md already
states that computing a requirement outside `resolveAuthRequirements` is how
enforcement and coverage silently diverge.

## 6. Testing

The happy path is the least of it. Each of these must fail when the
corresponding half of §4 is reverted:

- `ScopesAllOf` and `ScopesAnyOf` produce **different** `Requirement`s from
  identical SDL, and both differ from `ScopesNested`.
- A custom directive misplaced on a schema definition, a directive argument or
  an input field is a build error **naming the custom directive**.
- A malformed literal for a custom directive is a build error (gqlparser does
  not type-check directive argument literals; this is the gap
  `checkRequiresScopes` exists to close, and it must not apply to one spelling
  only).
- `maxRequirementGroups` caps a custom directive.
- A custom directive inherits through object types and interfaces exactly as
  `@requiresScopes` does.
- Both spellings on one coordinate AND.
- An undeclared `ScopeShape` is a build error.
- Declaring one name twice is a build error.
- Naming an argument the SDL's directive does not have is a build error, not a
  schema where every site silently carries no requirement.
- With no option supplied, behaviour is unchanged.

## 7. What this unblocks, and what it does not

With the core pluggable, `ext/authz` is no longer the package the parent design
imagined. `@requiresScopes` is already core; a legacy spelling is now a
one-liner in the consumer's own `NewSchema` call. What remains for `ext/authz`
is `@authenticated`, `@policy` and a batched `Guard` -- and whether those
justify a package is a question to answer against the built core, not to decide
here. This design deliberately leaves it open.
