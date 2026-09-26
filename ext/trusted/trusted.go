// Package trusted turns persisted queries into a safelist: the server
// executes only documents a build step registered, and refuses query text
// whatever hash accompanies it.
//
// That last part is the whole difference from ext/apq. Automatic persisted
// queries exist to save bandwidth, so they accept text that hashes correctly
// and remember it. A safelist exists to bound what the server will run, so
// accepting text would leave it decorative.
//
// A Store is an apq.Cache, so it goes into the transports' existing wiring:
//
//	gqlhttp.New(exec, gqlhttp.WithPersistedQueries(store))
//
// The wire format is the one automatic persisted queries already use,
// extensions.persistedQuery.sha256Hash, which is also how Apollo Router
// safelists. A top-level documentId field is not supported.
//
// # Enforcing on every transport
//
// WithPersistedQueries is a transport option, so the safelist binds only the
// handlers it was passed to: mount gqlws or gqlsse without it and those
// endpoints run any document. Store.Enforce closes that at the executor, which
// every transport shares:
//
//	exec := graphql.NewExecutor(schema, store.Enforce()...)
//
// It refuses any operation whose document text is not one the store holds,
// with the same PersistedQueryNotInList error the transports send. It checks
// what will run rather than how the client named it, so it composes with the
// transport wiring: a hash a transport resolved from the store yields
// registered text and passes. It is the boundary; the transport option is
// what lets clients send ids instead of text.
//
// Two limits, both from what an interceptor can reach. A query or mutation is
// refused before it is parsed, but a subscription only after parsing,
// validation and planning, since Executor.Subscribe runs no request
// interceptor: a non-safelisted subscription can still learn a validation
// error. And a streaming transport asks Executor.OperationKind what a document
// is before executing it, which parses it into the executor's bounded
// document cache whether or not it is later refused.
package trusted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
)

// Store is a fixed set of documents by id. It is read-only after
// construction and therefore safe to share between requests without a lock.
type Store struct {
	docs map[string]string
	// texts indexes docs by value, for Enforce: what an interceptor sees is
	// the document text, the id having been resolved (or never sent).
	texts map[string]struct{}
}

// NewStore holds docs as the complete set of documents this server will run.
//
// Ids are opaque: they are whatever the client sends as sha256Hash, so a
// generator that names documents by their hash and one that names them by an
// opaque build id both work, as long as both sides agree.
func NewStore(docs map[string]string) *Store {
	cp := make(map[string]string, len(docs))
	texts := make(map[string]struct{}, len(docs))
	for id, text := range docs {
		cp[id] = text
		texts[text] = struct{}{}
	}
	return &Store{docs: cp, texts: texts}
}

// LoadManifest reads either shape a generator emits: Apollo's persisted
// query manifest, or the flat id-to-text object Relay's --persist-output
// writes.
func LoadManifest(r io.Reader) (*Store, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var apollo struct {
		Operations []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(raw, &apollo); err == nil && apollo.Operations != nil {
		docs := make(map[string]string, len(apollo.Operations))
		for _, op := range apollo.Operations {
			docs[op.ID] = op.Body
		}
		return NewStore(docs), nil
	}
	var flat map[string]string
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("trusted: manifest is neither an Apollo manifest nor an id-to-document object: %w", err)
	}
	if flat == nil {
		return nil, errors.New("trusted: manifest is empty")
	}
	return NewStore(flat), nil
}

// LoadManifestFile is LoadManifest over a path, which is how a server reads
// the file its build step produced.
func LoadManifestFile(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return LoadManifest(f)
}

// Get returns the document registered under id.
func (s *Store) Get(id string) (string, bool) {
	q, ok := s.docs[id]
	return q, ok
}

// Set does nothing. A safelist that could be written to at request time
// would not be one.
func (s *Store) Set(string, string) {}

// TrustedDocuments marks this cache as a safelist rather than a cache. See
// apq.TrustedStore.
func (s *Store) TrustedDocuments() {}

// Len reports how many documents are registered, which is worth logging at
// start-up: a manifest that silently loaded zero documents refuses every
// request.
func (s *Store) Len() int { return len(s.docs) }

// Enforce returns executor options that refuse every operation whose document
// is not in the store, on whichever transport it arrived. See the package
// documentation for why this exists beside WithPersistedQueries and for its
// two limits.
func (s *Store) Enforce() []graphql.ExecutorOption {
	return []graphql.ExecutorOption{
		graphql.WithRequestInterceptor(graphql.RequestInterceptorFunc(
			func(ctx context.Context, req *graphql.Request, next graphql.RequestHandler) *graphql.Response {
				if !s.holds(req.Query) {
					return notInList()
				}
				return next(ctx, req)
			})),
		// Subscribe runs no request interceptor, so without this a
		// subscription would bypass the list entirely.
		graphql.WithSubscriptionInterceptor(graphql.SubscriptionInterceptorFunc(
			func(ctx context.Context, oc *graphql.OperationContext, next graphql.SubscriptionHandler) (<-chan *graphql.Response, error) {
				if !s.holds(oc.RawQuery) {
					return nil, &graphql.SubscribeError{Response: notInList()}
				}
				return next(ctx, oc)
			})),
	}
}

func (s *Store) holds(text string) bool {
	_, ok := s.texts[text]
	return ok
}

func notInList() *graphql.Response {
	err := graphql.Errorf("PersistedQueryNotInList").WithCode(apq.CodeNotInList)
	return &graphql.Response{Errors: []*graphql.Error{err}}
}
