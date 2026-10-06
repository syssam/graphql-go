package relay_test

import (
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/relay"
)

// The encoding is base64("Type:id") with standard padding, byte for byte
// what graphql-relay-js's btoa and graphql-java's Base64.getEncoder produce,
// so a client built against either talks to this server unchanged.
func TestToGlobalIDMatchesTheReferenceEncoding(t *testing.T) {
	if got := relay.ToGlobalID("User", "123"); got != graphql.ID("VXNlcjoxMjM=") {
		t.Fatalf("ToGlobalID = %q, want VXNlcjoxMjM=", got)
	}
}

func TestGlobalIDRoundTrips(t *testing.T) {
	typ, id, err := relay.FromGlobalID(relay.ToGlobalID("User", "123"))
	if err != nil || typ != "User" || id != "123" {
		t.Fatalf("FromGlobalID = (%q, %q, %v)", typ, id, err)
	}
}

// A local id may contain colons; only the first separates it from the type.
func TestGlobalIDKeepsColonsInTheLocalID(t *testing.T) {
	typ, id, err := relay.FromGlobalID(relay.ToGlobalID("Post", "2026:09:16"))
	if err != nil || typ != "Post" || id != "2026:09:16" {
		t.Fatalf("FromGlobalID = (%q, %q, %v)", typ, id, err)
	}
}

// One node has one id. Standard base64 decoding is lenient about line breaks
// and about the bits a final character does not use, so several strings
// decoded to the same node: a client cache keyed by id held it several times,
// and anything keyed by id on the server could be stepped around.
func TestFromGlobalIDAcceptsOnlyWhatToGlobalIDProduces(t *testing.T) {
	issued := string(relay.ToGlobalID("User", "123"))
	if issued != "VXNlcjoxMjM=" {
		t.Fatalf("fixture drifted: %q", issued)
	}
	for _, tc := range []struct{ name, gid string }{
		{"trailing newline", issued + "\n"},
		{"embedded line break", "VXNl\r\ncjoxMjM="},
		{"leading space", " " + issued},
		{"unused bits set in the last character", "VXNlcjoxMjN="},
		{"padding dropped", "VXNlcjoxMjM"},
		{"URL-safe alphabet", "UG9zdDo_Pz8="}, // "Post:???" is UG9zdDo/Pz8= in the standard one
	} {
		t.Run(tc.name, func(t *testing.T) {
			if typ, id, err := relay.FromGlobalID(graphql.ID(tc.gid)); err == nil {
				t.Fatalf("FromGlobalID(%q) = (%q, %q), want it refused: ToGlobalID never produces it", tc.gid, typ, id)
			}
		})
	}
	if typ, id, err := relay.FromGlobalID(relay.ToGlobalID("Post", "???")); err != nil || typ != "Post" || id != "???" {
		t.Fatalf("FromGlobalID = (%q, %q, %v)", typ, id, err)
	}
}

func TestFromGlobalIDRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, gid string }{
		{"not base64", "not-base64!!"},
		{"no separator", "VXNlcg=="}, // "User"
		{"empty type", "OjEyMw=="},   // ":123"
		{"empty id", "VXNlcjo="},     // "User:"
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := relay.FromGlobalID(graphql.ID(tc.gid)); err == nil {
				t.Fatalf("FromGlobalID(%q) accepted a malformed id", tc.gid)
			}
		})
	}
}
