# Guard a field and a type

Say in the schema who may read a field, a type and a row, and have the engine refuse, null
or drop whatever a caller may not have. The authorization types (`AuthShape`, `AuthSite`,
`Decision`, `Outcome`, `ObjectCheck`) are marked Experimental in their godoc, so their
shape may change before v1.

`@auth` is this schema's own directive. On a field it guards the field. On a type it is
added to every field of the type, so `Invoice.note` needs `user` and `staff`.
`@authorizeObject` is the engine's name for a type whose rows are decided one at a time.

```go
const billingSDL = `
directive @auth(requires: [String!]) on OBJECT | FIELD_DEFINITION
directive @authorizeObject on OBJECT

type Invoice @auth(requires: ["user"]) @authorizeObject {
  id: ID!
  total: Int!
  note: String @auth(requires: ["staff"])
}

type Query {
  invoices: [Invoice!]! @auth(requires: ["user"])
  invoice(id: ID!): Invoice @auth(requires: ["user"])
}
`
```

One schema option makes `@auth` a requirement and says how to read its list: `ScopesAllOf`
means every scope named is needed.

```go
graphql.RequirementDirective("auth", "requires", graphql.ScopesAllOf),
```

Fields are decided once per operation, before any resolver runs. `callerFrom(ctx)` is
ordinary context plumbing: it returns the `Caller` your HTTP middleware stored with
`WithCaller` after checking a token, and a request with none holds no scopes.

```go
func authorize(ctx context.Context, shape *graphql.AuthShape, d *graphql.Decision) error {
	held := callerFrom(ctx).Scopes
	for i, site := range shape.Sites() {
		// A row does not exist yet: AuthorizeObjects decides it, below.
		if site.Kind == graphql.SiteInstance || site.Requires.Satisfied(held) {
			continue
		}
		outcome := graphql.Deny(strings.Join(site.Requires.Scopes(), " and "), site.Coord)
		if site.Coord == "Invoice.note" {
			// Nullable, so it can be withheld without failing the invoice.
			outcome = graphql.Null()
		}
		if err := d.Set(i, outcome); err != nil {
			return err
		}
	}
	return nil
}
```

Rows are decided during execution, a whole list in one call, because which invoices are
yours depends on the rows and not on the query.

```go
func (invoiceRows) AuthorizeObjects(ctx context.Context, checks []graphql.ObjectCheck) ([]graphql.Outcome, error) {
	caller := callerFrom(ctx)
	// One outcome per check, in the same order. The zero Outcome allows.
	outcomes := make([]graphql.Outcome, len(checks))
	for i, check := range checks {
		invoice, ok := check.Object.(*Invoice)
		switch {
		case ok && (caller.Scopes["staff"] || invoice.Owner == caller.Name):
			// Theirs to see.
		case check.Site.ListElement:
			outcomes[i] = graphql.Drop()
		case !check.Site.NonNull:
			outcomes[i] = graphql.Null()
		default:
			outcomes[i] = graphql.Deny("read", check.Type)
		}
	}
	return outcomes, nil
}
```

| Outcome | The client gets | Where |
|---|---|---|
| `Deny` | `null` and an error with code `FORBIDDEN`; a non-null field takes its parent with it | anywhere |
| `Null` | `null` and no error, as if there were nothing there | at a nullable position |
| `Drop` | a list without that row, and no error | for a row in a list |

Two executor options do the enforcing.

```go
func NewBillingExecutor(schema *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(schema,
		graphql.WithAuthorizer(graphql.AuthorizerFunc(authorize)),
		graphql.WithObjectAuthorizer(invoiceRows{}),
	)
}
```

## Check it worked

Test the refusal, as the caller who should get it. `ask` executes a query with a `Caller`
in the context and returns the data and the errors.

```go
func TestAnonymousCallerIsRefused(t *testing.T) {
	data, errs := ask(t, billingExecutor(t), anonymous, `{ invoices { id total } }`)

	if len(errs) != 1 || errs[0].Extensions["code"] != graphql.CodeForbidden {
		t.Fatalf("errors = %v, want one %s", errs, graphql.CodeForbidden)
	}
	// Query.invoices is non-null, so refusing it leaves no data at all.
	if data != "null" {
		t.Fatalf("a refused request still carried data: %s", data)
	}
}
```

`TestUserIsServedTheirOwnInvoices` is the other side, so the suite cannot pass by refusing
everyone: Ada gets her two invoices with `note` null, and Grace's is not in the list.

```sh
go test -race -run 'Anonymous|User|Staff|SomeoneElse|Enforced|BuildError' ./examples/howto/
```

## What goes wrong

- **`NewSchema` fails: `Query.secret(hint:): @auth is not enforced on an argument; declare
  it on an object, interface or field`.** By design. Requirements are compiled into each
  query's plan, which has only objects, interfaces and fields; anywhere else the directive
  could never run, so the build refuses it. Move it to the field.
- **`NewSchema` fails: `Query.secret: @auth requires must be a non-empty list of
  strings`.** The SDL says `requires: []`, which every caller would satisfy. Name a scope
  or remove the directive.
- **Every caller is served everything.** The executor lacks `WithAuthorizer`, or
  `WithObjectAuthorizer` for the rows. Directives alone enforce nothing, which is why the
  test asserts a refusal.
- **`-race` reports a data race in `AuthorizeObjects`.** It is called concurrently within
  one request, once for each guarded list, so a cache inside it needs a mutex or a
  `sync.Map`.

## Next

- [`examples/storefront`](../../examples/README.md#authorization-in-storefront): four
  principals over one graph, with `@authorizeInput`, `Redact` and `RequireAuthCoverage`.
- [`docs/operations.md`](../operations.md#authorization-in-production): what a remote
  policy backend costs in round trips, and how it fails.
