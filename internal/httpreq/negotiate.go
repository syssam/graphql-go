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
	// The most specific range that covers a type decides for it: the type
	// itself, then application/*, then */*. So a type named with q=0 is
	// refused whatever a wildcard beside it says, and so is application/*;q=0
	// beside a */* -- "application/json;q=0, */*" asks for anything but JSON
	// and used to be answered with JSON.
	const (
		exactJSON = iota
		exactGraphQL
		anyApplication
		anything
		ranges
	)
	var q [ranges]float64
	var named [ranges]bool
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		i := -1
		switch mt {
		case MediaTypeJSON:
			i = exactJSON
		case MediaTypeGraphQLResponse:
			i = exactGraphQL
		case "application/*":
			i = anyApplication
		case "*/*":
			i = anything
		}
		if i < 0 {
			continue
		}
		w := 1.0
		if qs, ok := params["q"]; ok {
			w = parseQ(qs)
		}
		// A range given twice takes its better weight.
		if !named[i] || w > q[i] {
			q[i], named[i] = w, true
		}
	}
	weigh := func(exact int) (w float64, explicit bool) {
		for _, i := range []int{exact, anyApplication, anything} {
			if named[i] {
				return q[i], i == exact
			}
		}
		return 0, false
	}
	jsonQ, _ := weigh(exactJSON)
	graphQLQ, graphQLExplicit := weigh(exactGraphQL)
	switch {
	case jsonQ <= 0 && graphQLQ <= 0:
		return MediaTypeGraphQLResponse, false
	case jsonQ != graphQLQ:
		if jsonQ > graphQLQ {
			return MediaTypeJSON, true
		}
		return MediaTypeGraphQLResponse, true
	// Equal weights. A type the client named outranks one a wildcard let in,
	// whichever order they arrived in; between two it named, the
	// specification's preferred type wins.
	case graphQLExplicit:
		return MediaTypeGraphQLResponse, true
	}
	// Both by wildcard, or JSON alone named. A wildcard names neither type, so
	// it is answered like a missing Accept: application/json, which the audit
	// requires ("SHOULD accept */* and use application/json for the
	// content-type").
	return MediaTypeJSON, true
}

// MediaTypeEventStream is the type of a Server-Sent Events response.
const MediaTypeEventStream = "text/event-stream"

// AcceptsEventStream reports whether the client will take a stream. An absent
// header is taken as yes so that command-line clients work; a header that
// names only other types is a client pointed at the wrong endpoint. The most
// specific range that matches decides, so "text/event-stream;q=0, */*" is a
// refusal and "text/*;q=0, text/event-stream" is not.
func AcceptsEventStream(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}
	// Indexed by specificity: the type itself, text/*, */*.
	var seen, accepted [3]bool
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		i := -1
		switch mt {
		case MediaTypeEventStream:
			i = 0
		case "text/*":
			i = 1
		case "*/*":
			i = 2
		}
		if i < 0 {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			q = parseQ(qs)
		}
		seen[i] = true
		accepted[i] = accepted[i] || q > 0
	}
	for i := range seen {
		if seen[i] {
			return accepted[i]
		}
	}
	return false
}

// parseQ reads an RFC 9110 qvalue, "0" [ "." 0*3DIGIT ] / "1" [ "." 0*3("0") ],
// and answers 0 for anything else. An unreadable weight used to count as
// q=1, which promoted a range the client may have meant to refuse -- "q=O"
// for "q=0" -- to its first choice. Treating it as not acceptable fails
// closed: the range is ignored, and another the client named can still win.
// strconv.ParseFloat alone is too loose here: it takes "2", "1e0", "NaN" and
// "Inf".
func parseQ(s string) float64 {
	whole, frac, _ := strings.Cut(s, ".")
	if (whole != "0" && whole != "1") || len(frac) > 3 {
		return 0
	}
	for _, c := range frac {
		if c < '0' || c > '9' || (whole == "1" && c != '0') {
			return 0
		}
	}
	q, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return q
}
