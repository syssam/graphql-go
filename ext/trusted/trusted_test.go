package trusted_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/ext/trusted"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const doc = `{ hello }`

func newStore() *trusted.Store {
	return trusted.NewStore(map[string]string{apq.Hash(doc): doc})
}

func persisted(hash string) map[string]any {
	return map[string]any{"persistedQuery": map[string]any{"version": float64(1), "sha256Hash": hash}}
}

func codeOf(t *testing.T, resp *graphql.Response) string {
	t.Helper()
	if resp == nil {
		t.Fatal("request was allowed through")
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v", resp.Errors)
	}
	code, _ := resp.Errors[0].Extensions["code"].(string)
	return code
}

func TestRegisteredDocumentResolves(t *testing.T) {
	req := &graphql.Request{Extensions: persisted(apq.Hash(doc))}
	if resp := apq.Resolve(newStore(), req); resp != nil {
		t.Fatalf("rejected a registered document: %v", resp.Errors)
	}
	if req.Query != doc {
		t.Fatalf("query = %q", req.Query)
	}
}

func TestUnknownIDIsNotFound(t *testing.T) {
	req := &graphql.Request{Extensions: persisted(apq.Hash(`{ other }`))}
	if got := codeOf(t, apq.Resolve(newStore(), req)); got != apq.CodeNotFound {
		t.Fatalf("code = %q, want %q", got, apq.CodeNotFound)
	}
}

// This is the case that separates a safelist from a cache. Automatic
// persisted queries accept query text that hashes correctly and register it;
// a safelist must refuse, or any client can run anything it likes.
func TestQueryTextWithACorrectHashIsStillRefused(t *testing.T) {
	const evil = `{ hello }  # anything at all`
	req := &graphql.Request{Query: evil, Extensions: persisted(apq.Hash(evil))}
	if got := codeOf(t, apq.Resolve(newStore(), req)); got != apq.CodeNotInList {
		t.Fatalf("code = %q, want %q", got, apq.CodeNotInList)
	}
}

// A plain request with no persistedQuery extension at all is the obvious way
// round a safelist, so it is refused too.
func TestFreeformQueryIsRefused(t *testing.T) {
	req := &graphql.Request{Query: `{ hello }`}
	if got := codeOf(t, apq.Resolve(newStore(), req)); got != apq.CodeNotInList {
		t.Fatalf("code = %q, want %q", got, apq.CodeNotInList)
	}
}

// Registration is not merely ignored, it must leave no trace: a refused
// document does not become runnable on the next request.
func TestRefusedDocumentIsNotLearned(t *testing.T) {
	s := newStore()
	const evil = `{ hello } # sneak`
	apq.Resolve(s, &graphql.Request{Query: evil, Extensions: persisted(apq.Hash(evil))})
	req := &graphql.Request{Extensions: persisted(apq.Hash(evil))}
	if got := codeOf(t, apq.Resolve(s, req)); got != apq.CodeNotFound {
		t.Fatalf("code = %q, want %q — the store learned a document", got, apq.CodeNotFound)
	}
}

func TestLoadManifestReadsTheApolloFormat(t *testing.T) {
	src := `{"format":"apollo-persisted-query-manifest","version":1,"operations":[
		{"id":"abc","body":"{ hello }","name":"Hello","type":"query"}]}`
	s, err := trusted.LoadManifest(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("abc"); !ok || got != "{ hello }" {
		t.Fatalf("Get = (%q, %v)", got, ok)
	}
}

// Relay's --persist-output writes a flat id-to-text object instead.
func TestLoadManifestReadsAFlatMap(t *testing.T) {
	s, err := trusted.LoadManifest(strings.NewReader(`{"abc":"{ hello }"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("abc"); !ok || got != "{ hello }" {
		t.Fatalf("Get = (%q, %v)", got, ok)
	}
}

func TestLoadManifestRejectsRubbish(t *testing.T) {
	if _, err := trusted.LoadManifest(strings.NewReader(`not json`)); err == nil {
		t.Fatal("accepted a manifest that is not JSON")
	}
}

// The store drops into the transports' existing persisted-query wiring, so
// the safelist is enforced on the real request path and not only in a unit.
func TestSafelistIsEnforcedOverHTTP(t *testing.T) {
	s, err := graphql.NewSchema(graphql.SDL(`type Query { hello: String! }`),
		graphql.Query(graphql.Resolve("hello", func(context.Context, graphql.Root) (string, error) {
			return "hi", nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	h := gqlhttp.New(graphql.NewExecutor(s), gqlhttp.WithPersistedQueries(newStore()))

	post := func(body map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
		r.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %q: %v", w.Body.String(), err)
		}
		return out
	}

	ok := post(map[string]any{"extensions": persisted(apq.Hash(doc))})
	if data, _ := ok["data"].(map[string]any); data["hello"] != "hi" {
		t.Fatalf("registered document did not run: %v", ok)
	}

	refused := post(map[string]any{"query": `{ hello }`})
	errs, _ := refused["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("freeform query was not refused: %v", refused)
	}
	ext, _ := errs[0].(map[string]any)["extensions"].(map[string]any)
	if ext["code"] != apq.CodeNotInList {
		t.Fatalf("code = %v, want %s", ext["code"], apq.CodeNotInList)
	}
}

// LoadManifestFile is how a server reads what its build step wrote, and Len
// is what tells it the file was not empty — a manifest that silently loaded
// nothing refuses every request.
func TestLoadManifestFileReadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	body := `{"` + apq.Hash(doc) + `":` + strconv.Quote(doc) + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := trusted.LoadManifestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	req := &graphql.Request{Extensions: persisted(apq.Hash(doc))}
	if resp := apq.Resolve(s, req); resp != nil {
		t.Fatalf("a document from the file was refused: %v", resp.Errors)
	}
	if req.Query != doc {
		t.Fatalf("query = %q", req.Query)
	}
}

func TestLoadManifestFileReportsAMissingFile(t *testing.T) {
	if _, err := trusted.LoadManifestFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing manifest was accepted")
	}
}

// The safelist is guarded twice on purpose, and only one half was tested.
// apq.Resolve refuses query text against a TrustedStore before it would ever
// call Set, and Store.Set is a no-op even if something did. Break either one
// alone and every other test in this repository stays green, so a green suite
// says nothing about whether the second half is still there.
//
// The half this covers is the one that holds if the first is ever refactored:
// a client that reaches Set must not be able to add to the safelist, which
// would let it register and then run any document it likes.
func TestStoreSetCannotAddToTheSafelist(t *testing.T) {
	s, err := trusted.LoadManifest(strings.NewReader(`{"abc":"{ __typename }"}`))
	if err != nil {
		t.Fatal(err)
	}
	before := s.Len()

	s.Set("evil", "{ secrets }")
	s.Set("abc", "{ somethingElse }")

	if s.Len() != before {
		t.Errorf("Len = %d, was %d: Set added to the safelist", s.Len(), before)
	}
	if _, ok := s.Get("evil"); ok {
		t.Error("a document registered through Set is now in the safelist")
	}
	if q, _ := s.Get("abc"); q != "{ __typename }" {
		t.Errorf("Set replaced a safelisted document with %q", q)
	}
}
