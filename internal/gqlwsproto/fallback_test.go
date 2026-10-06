package gqlwsproto

import (
	"encoding/json"
	"testing"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// This package repeats two of httpreq's bodies rather than import it, so that
// the protocol depends on nothing but the root package. A client that moves
// between transports must still read the same failure the same way, and
// nothing but this holds the copies to the originals.
func TestTheFallbacksAreTheHTTPHandlers(t *testing.T) {
	if fallbackPayload != httpreq.FallbackBody {
		t.Errorf("fallbackPayload = %s, want httpreq.FallbackBody, %s", fallbackPayload, httpreq.FallbackBody)
	}

	got, err := json.Marshal([]any{internalError()})
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(httpreq.InternalError().Errors)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("internalError() = %s, want httpreq.InternalError's, %s", got, want)
	}
}
