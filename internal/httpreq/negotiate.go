package httpreq

import (
	"mime"
	"strconv"
	"strings"
)

// MediaTypeGraphQLResponse is the type the GraphQL over HTTP specification
// prefers, and the one a request error's 400 status is defined against.
const MediaTypeGraphQLResponse = "application/graphql-response+json"

// Negotiate chooses the response media type from an Accept header. The
// second result is false when the client accepts neither supported type.
//
// It lives here rather than in a transport because the choice is
// client-visible twice over: it names the response Content-Type, and it
// decides whether a request error is answered with 400 or with 200.
func Negotiate(accept string) (string, bool) {
	if strings.TrimSpace(accept) == "" {
		return MediaTypeGraphQLResponse, true
	}
	best, bestQ := "", -1.0
	consider := func(mt string, q float64) {
		// Ties favour the specification's preferred type.
		if q > bestQ || (q == bestQ && mt == MediaTypeGraphQLResponse) {
			best, bestQ = mt, q
		}
	}
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			if parsed, err := strconv.ParseFloat(qs, 64); err == nil {
				q = parsed
			}
		}
		if q <= 0 {
			continue
		}
		switch mt {
		case MediaTypeGraphQLResponse, MediaTypeJSON:
			consider(mt, q)
		case "*/*", "application/*":
			// Wildcards match both; the preferred type wins the tie, but an
			// explicit application/json still outranks a wildcard at equal q.
			if q > bestQ {
				best, bestQ = MediaTypeGraphQLResponse, q
			}
		}
	}
	if best == "" {
		return MediaTypeGraphQLResponse, false
	}
	return best, true
}
