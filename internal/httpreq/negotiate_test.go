package httpreq_test

import (
	"testing"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// Negotiate decides two client-visible things at once: the response
// Content-Type, and -- because the transports key on it -- whether a request
// error is answered 200 or 400. The GraphQL over HTTP audit suite
// (graphql-http) requires the first two rows below; this table is what keeps
// them true without Node.
func TestNegotiate(t *testing.T) {
	const (
		gql  = httpreq.MediaTypeGraphQLResponse
		json = httpreq.MediaTypeJSON
	)
	for _, tc := range []struct {
		accept string
		want   string
		ok     bool
		why    string
	}{
		{"", json, true, "SHOULD assume application/json content-type when accept is missing"},
		{"   ", json, true, "whitespace is not a media type"},
		{"*/*", json, true, "SHOULD accept */* and use application/json for the content-type"},
		{"application/*", json, true, "a wildcard names neither type"},
		{"text/html, application/*", json, true, "the unsupported type is ignored, the wildcard decides"},

		{json, json, true, "explicit"},
		{gql, gql, true, "explicit"},

		// An explicit type outranks a wildcard whichever order they arrive in,
		// which is the case a naive "last one wins" gets wrong in one
		// direction and "first one wins" in the other.
		{"*/*, " + gql, gql, true, "explicit beats a wildcard it follows"},
		{gql + ", */*", gql, true, "explicit beats a wildcard it precedes"},
		{"*/*, " + json, json, true, "wildcard already yields json, so this is still json"},

		// Both named: the specification's preferred type wins a tie, q-values
		// win outright.
		{json + ", " + gql, gql, true, "tie goes to the preferred type"},
		{gql + ";q=0.5, " + json, json, true, "q-value beats the tie-break"},
		{json + ";q=0.5, " + gql + ";q=0.9", gql, true, "q-value"},
		{"application/*;q=0.8, " + json + ";q=0.9", json, true, "explicit q beats wildcard q"},

		// q=0 is a refusal, not a low preference.
		{gql + ";q=0, " + json, json, true, "q=0 refuses that type"},

		{"text/html", gql, false, "nothing acceptable"},
		{"text/html, application/xml", gql, false, "nothing acceptable"},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			got, ok := httpreq.Negotiate(tc.accept)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Negotiate(%q) = %q, %v; want %q, %v (%s)",
					tc.accept, got, ok, tc.want, tc.ok, tc.why)
			}
		})
	}
}
