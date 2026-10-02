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
	jsonQ, _ := weigh(accept, MediaTypeJSON)
	graphQLQ, graphQLExplicit := weigh(accept, MediaTypeGraphQLResponse)
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

// weigh is the weight an Accept header gives one application/* media type,
// and whether the header named it rather than covering it with a range. The
// most specific range that covers a type decides for it: the type itself,
// then application/*, then */*. So a type named with q=0 is refused whatever
// a wildcard beside it says, and so is application/*;q=0 beside a */* --
// "application/json;q=0, */*" asks for anything but JSON and used to be
// answered with JSON. A range given twice takes its better weight.
func weigh(accept, mediaType string) (q float64, explicit bool) {
	const (
		exact = iota
		anyApplication
		anything
		ranges
	)
	var weight [ranges]float64
	var named [ranges]bool
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		i := -1
		switch mt {
		case mediaType:
			i = exact
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
		if !named[i] || w > weight[i] {
			weight[i], named[i] = w, true
		}
	}
	for i := range named {
		if named[i] {
			return weight[i], i == exact
		}
	}
	return 0, false
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

// PrefersEventStream reports whether a request should be answered as a
// stream by an endpoint that can answer either way. It is yes only when the
// client names text/event-stream and weighs it above JSON. JSON's weight is
// read as Negotiate reads it, ranges included, so "*/*, text/event-stream;
// q=0.1" is a client that would rather have anything else. At one weight the
// stream wins against a wildcard, which it was named beside, and loses to a
// JSON type the client named too: that answers a query, and a stream does not
// answer a client that cannot read one.
func PrefersEventStream(accept string) bool {
	stream := 0.0
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || mt != MediaTypeEventStream {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			q = parseQ(qs)
		}
		stream = max(stream, q)
	}
	if stream <= 0 {
		return false
	}
	for _, mediaType := range []string{MediaTypeJSON, MediaTypeGraphQLResponse} {
		if q, explicit := weigh(accept, mediaType); q > stream || (q == stream && explicit) {
			return false
		}
	}
	return true
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
