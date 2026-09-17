package graphql

import (
	"strings"
	"testing"
)

// Validation errors volunteer schema details that introspection would have
// withheld: field names, type names, argument names and input field names.
// With introspection disabled the schema is still enumerable one typo at a
// time, so the suggestions need their own switch.
//
// One case per gqlparser rule that suggests. leaks is the whole suggestion
// clause rather than the bare name, because the name is often a substring of
// the typo that provoked it ("idx" contains "id").
var suggestionLeaks = []struct {
	rule     string
	query    string
	leaks    string // the clause the default rules volunteer
	stillHas string // the error must survive, stripped of the clause
}{
	{
		rule:     "FieldsOnCorrectType",
		query:    `{ me { nam } }`,
		leaks:    `Did you mean "name"?`,
		stillHas: `Cannot query field "nam" on type "User".`,
	},
	{
		rule:     "ScalarLeafs",
		query:    `{ me { pet } }`,
		leaks:    `Did you mean "pet { ... }"?`,
		stillHas: "must have a selection of subfields",
	},
	{
		rule:     "KnownArgumentNames",
		query:    `{ user(idx: "1") { id } }`,
		leaks:    `Did you mean "id"?`,
		stillHas: `Unknown argument "idx" on field "Query.user".`,
	},
	{
		rule:     "KnownTypeNames",
		query:    `fragment F on Usr { id } { me { ...F } }`,
		leaks:    `Did you mean "User"?`,
		stillHas: `Unknown type "Usr".`,
	},
	{
		rule:     "ValuesOfCorrectType",
		query:    `{ users(filter: {nam: "x"}) { id } }`,
		leaks:    `Did you mean "name"?`,
		stillHas: `Field "nam" is not defined by type "Filter".`,
	},
}

// The default rules are the positive control. Without it, the test below
// would pass against an executor that stopped reporting the error at all,
// and against a rule that never suggested in the first place — two wrong
// assumptions this control has already caught once.
func TestDefaultRulesDiscloseSchemaDetails(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for _, tc := range suggestionLeaks {
		t.Run(tc.rule, func(t *testing.T) {
			msg := validationMessage(t, e, tc.query)
			if !strings.Contains(msg, tc.leaks) {
				t.Fatalf("expected the default rules to disclose %q\n got: %s", tc.leaks, msg)
			}
		})
	}
}

func TestDisableSuggestionsWithholdsSchemaDetails(t *testing.T) {
	_, e := newFixtureExecutor(t, DisableSuggestions())
	for _, tc := range suggestionLeaks {
		t.Run(tc.rule, func(t *testing.T) {
			msg := validationMessage(t, e, tc.query)
			if strings.Contains(msg, tc.leaks) {
				t.Errorf("error discloses %q\n got: %s", tc.leaks, msg)
			}
			if strings.Contains(msg, "Did you mean") {
				t.Errorf("error still carries a suggestion clause\n got: %s", msg)
			}
			if !strings.Contains(msg, tc.stillHas) {
				t.Errorf("error no longer reports the problem; want it to contain %q\n got: %s", tc.stillHas, msg)
			}
		})
	}
}

// Suppressing suggestions must not suppress the introspection rule: an
// operation that reaches __schema is still rejected when introspection is
// off, including when it is composed with ordinary fields. See
// github.com/graphql-hive/envelop#892, where skipping the check for
// introspection documents let `{ __schema { __typename } secret }` through.
func TestDisableSuggestionsKeepsIntrospectionRejected(t *testing.T) {
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), append(f.options(), DisableIntrospection())...)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	e := NewExecutor(s, DisableSuggestions())
	msg := validationMessage(t, e, `{ __schema { __typename } me { id } }`)
	if !strings.Contains(msg, "introspection is not allowed") {
		t.Fatalf("introspection composed into a normal operation was not rejected\n got: %s", msg)
	}
}

func validationMessage(t *testing.T, e *Executor, query string) string {
	t.Helper()
	resp := run(t, e, query, "")
	if len(resp.Errors) == 0 {
		t.Fatalf("query %s produced no error", query)
	}
	return resp.Errors[0].Message
}
