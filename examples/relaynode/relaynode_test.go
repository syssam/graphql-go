package relaynode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
)

func newExecutor(t testing.TB) *graphql.Executor {
	t.Helper()
	s, _, err := New()
	if err != nil {
		t.Fatalf("building schema: %v", err)
	}
	return graphql.NewExecutor(s)
}

func run(t testing.TB, e *graphql.Executor, query, vars string) *graphql.Response {
	t.Helper()
	req := &graphql.Request{Query: query}
	if vars != "" {
		req.Variables = json.RawMessage(vars)
	}
	return e.Execute(context.Background(), req)
}

func data(t testing.TB, resp *graphql.Response, want string) {
	t.Helper()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}
	if got := string(resp.Data); got != want {
		t.Fatalf("data =\n%s\nwant\n%s", got, want)
	}
}

// The encoding is the interoperability promise: base64("Type:id"), the same
// bytes graphql-relay-js and graphql-java produce. A client that caches by
// global id, or a Relay store that re-fetches one, does not care which server
// wrote it -- which is only true while this holds exactly.
func TestGlobalIDIsTheRelayEncoding(t *testing.T) {
	gid := GlobalID("User", "1")
	if want := graphql.ID(base64.StdEncoding.EncodeToString([]byte("User:1"))); gid != want {
		t.Fatalf("global id = %q, want %q", gid, want)
	}
	// And the id field emits it, rather than the local id. Returning the local
	// id there is the quiet failure this pins: it is a valid ID! and node(id:)
	// then fails for every object.
	e := newExecutor(t)
	data(t, run(t, e, `{ users(first: 1) { edges { node { id name } } } }`, ""),
		`{"users":{"edges":[{"node":{"id":"`+string(gid)+`","name":"Ada Lovelace"}}]}}`)
}

// The round trip Relay is built on: an id from one response is handed back to
// node(id:) and comes out as the same object, with no knowledge of which type
// it was.
func TestNodeRefetchesEachType(t *testing.T) {
	e := newExecutor(t)
	const q = `query($id: ID!) { node(id: $id) { __typename ... on User { name } ... on Repository { name stars } } }`

	data(t, run(t, e, q, `{"id":"`+string(GlobalID("User", "2"))+`"}`),
		`{"node":{"__typename":"User","name":"Grace Hopper"}}`)

	data(t, run(t, e, q, `{"id":"`+string(GlobalID("Repository", "12"))+`"}`),
		`{"node":{"__typename":"Repository","name":"cobol","stars":1863}}`)
}

// Three ways for node(id:) to find nothing, which must not be three different
// kinds of answer. A well-formed id naming a type this server does not serve,
// and one naming a missing row, are both a null Node -- the id decoded, it
// just named nothing. Text that is not a global id at all is an error, because
// the client sent something it could not have got from this server.
func TestNodeDistinguishesUnknownFromMalformed(t *testing.T) {
	e := newExecutor(t)
	const q = `query($id: ID!) { node(id: $id) { __typename } }`

	for _, id := range []string{
		string(GlobalID("Repository", "999")), // right type, no such row
		string(GlobalID("Organisation", "1")), // a type this server does not serve
	} {
		resp := run(t, e, q, `{"id":"`+id+`"}`)
		data(t, resp, `{"node":null}`)
	}

	resp := run(t, e, q, `{"id":"not-a-global-id"}`)
	if len(resp.Errors) == 0 {
		t.Fatalf("a malformed global id was accepted: %s", resp.Data)
	}
}

// Forward pagination: first/after walks the list and reports hasNextPage
// honestly at the boundary, which is the bit a hand-rolled connection usually
// gets wrong on the last page.
func TestForwardPaginationWalksToTheEnd(t *testing.T) {
	e := newExecutor(t)
	const q = `query($first: Int, $after: String) {
		repositories(first: $first, after: $after) {
			edges { cursor node { name } }
			pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
		}
	}`

	var seen []string
	var after string
	for page := 0; page < 10; page++ {
		vars := `{"first":2}`
		if after != "" {
			vars = `{"first":2,"after":` + jsonString(after) + `}`
		}
		resp := run(t, e, q, vars)
		if len(resp.Errors) > 0 {
			t.Fatalf("page %d: %v", page, resp.Errors[0])
		}
		var got struct {
			Repositories struct {
				Edges []struct {
					Cursor string `json:"cursor"`
					Node   struct {
						Name string `json:"name"`
					} `json:"node"`
				} `json:"edges"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal(resp.Data, &got); err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, edge := range got.Repositories.Edges {
			seen = append(seen, edge.Node.Name)
		}
		if !got.Repositories.PageInfo.HasNextPage {
			break
		}
		if got.Repositories.PageInfo.EndCursor == nil {
			t.Fatal("hasNextPage is true with no endCursor to continue from")
		}
		after = *got.Repositories.PageInfo.EndCursor
	}

	want := "analytical-engine,note-g,cobol,flow-matic,nanosecond"
	if strings.Join(seen, ",") != want {
		t.Fatalf("paged through %q, want %q", strings.Join(seen, ","), want)
	}
}

// A connection nested under an object, which is where the four arguments have
// to be decoded against the parent rather than the root.
func TestConnectionUnderAnObject(t *testing.T) {
	e := newExecutor(t)
	data(t, run(t, e, `query($id: ID!) { node(id: $id) { ... on User { repositories(first: 1) { edges { node { name } } pageInfo { hasNextPage } } } } }`,
		`{"id":"`+string(GlobalID("User", "2"))+`"}`),
		`{"node":{"repositories":{"edges":[{"node":{"name":"cobol"}}],"pageInfo":{"hasNextPage":true}}}}`)
}

// Describe is the only place anyone should decode a global id by hand, so it
// had better agree with the encoder.
func TestDescribeRoundTrips(t *testing.T) {
	got, err := Describe(GlobalID("Repository", "10"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Repository#10" {
		t.Fatalf("Describe = %q, want Repository#10", got)
	}
	if _, err := Describe("not-a-global-id"); err == nil {
		t.Fatal("Describe accepted text that is not a global id")
	}
}

// jsonString quotes a cursor for the variables payload; cursors are opaque
// text and must not be pasted in raw.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
