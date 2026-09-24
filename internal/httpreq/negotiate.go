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
	// A client that named no type is not asking for the newer one, and the
	// media type decides the status of a request error as well as the header,
	// so answering graphql-response+json here would also turn a 200 into a
	// 400 for a client that never opted in. graphql-http (the specification's
	// reference implementation), graphql-yoga and Apollo Server all answer
	// application/json, and the specification's own audit suite requires it:
	// "SHOULD assume application/json content-type when accept is missing".
	if strings.TrimSpace(accept) == "" {
		return MediaTypeJSON, true
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
			// A wildcard names neither type, so it is answered like a missing
			// Accept: application/json, which the audit requires ("SHOULD
			// accept */* and use application/json for the content-type"). An
			// explicit type still outranks a wildcard at equal q, in either
			// direction -- consider's tie-break picks graphql-response+json
			// over the json a wildcard already put there.
			if q > bestQ {
				best, bestQ = MediaTypeJSON, q
			}
		}
	}
	if best == "" {
		return MediaTypeGraphQLResponse, false
	}
	return best, true
}
