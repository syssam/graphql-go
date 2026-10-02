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
		// A refused type stays refused when a wildcard follows: the wildcard
		// is "anything else", and the client has said what else is not.
		{json + ";q=0, */*", gql, true, "a wildcard does not bring back a refused type"},
		{"*/*, " + json + ";q=0", gql, true, "nor when the refusal comes second"},
		{gql + ";q=0, */*", json, true, "the other one refused"},
		{json + ";q=0, " + gql + ";q=0, */*", gql, false, "both refused: the wildcard has nothing left"},
		// The most specific range that covers a type decides for it, so a
		// refusal of application/* is not undone by the */* beside it.
		{"application/*;q=0, */*", gql, false, "application/* refuses both types whatever */* says"},
		{"*/*, application/*;q=0", gql, false, "nor in the other order"},
		{"application/*;q=0, " + json, json, true, "an explicit type outranks the range that refuses it"},
		{"*/*;q=0, application/*", json, true, "application/* outranks */*"},
		{"*/*;q=0, " + gql, gql, true, "an explicit type outranks */*"},
		{"application/*;q=0.2, */*;q=0.9", json, true, "application/* decides, at its own weight"},

		// RFC 9110: a weight outside the qvalue grammar does not make the
		// range acceptable. It used to count as q=1, so a typo for q=0 made
		// the refused type the client's first choice.
		{gql + ";q=O, " + json + ";q=0.5", json, true, "unparseable q is not acceptable"},
		{gql + ";q=2, " + json + ";q=0.5", json, true, "q above 1 is outside the grammar"},
		{gql + ";q=NaN, " + json + ";q=0.5", json, true, "ParseFloat takes NaN; the grammar does not"},
		{gql + ";q=1e0, " + json + ";q=0.5", json, true, "ParseFloat takes exponents; the grammar does not"},
		{gql + ";q=0.0001", gql, false, "at most three decimals"},
		{gql + ";q=1.001, " + json + ";q=0.5", json, true, "1 takes only zero decimals"},
		{"*/*;q=bogus", gql, false, "an unparseable wildcard accepts nothing"},
		{json + ";q=1., " + gql + ";q=0.999", json, true, "1. is inside the grammar"},
		{json + ";q=0.123, " + gql + ";q=0.12", json, true, "three decimals are allowed"},

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

// The stream handlers asked only whether the header named a stream type and
// ignored its weight, so a client that refused one was sent one.
func TestAcceptsEventStream(t *testing.T) {
	for accept, want := range map[string]bool{
		"":                                true,
		"text/event-stream":               true,
		"text/*":                          true,
		"*/*":                             true,
		"application/json":                false,
		"text/event-stream;q=0.5":         true,
		"text/event-stream;q=0":           false,
		"text/event-stream;q=0, */*":      false,
		"*/*, text/event-stream;q=0":      false,
		"*/*;q=0":                         false,
		"text/*;q=0, text/event-stream":   true,
		"application/json, text/*;q=0.1":  true,
		"text/event-stream;q=bogus":       false,
		"TEXT/EVENT-STREAM":               true,
		"text/event-stream; charset=utf8": true,
	} {
		if got := httpreq.AcceptsEventStream(accept); got != want {
			t.Errorf("AcceptsEventStream(%q) = %t, want %t", accept, got, want)
		}
	}
}
