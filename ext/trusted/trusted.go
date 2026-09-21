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
package trusted

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Store is a fixed set of documents by id. It is read-only after
// construction and therefore safe to share between requests without a lock.
type Store struct {
	docs map[string]string
}

// NewStore holds docs as the complete set of documents this server will run.
//
// Ids are opaque: they are whatever the client sends as sha256Hash, so a
// generator that names documents by their hash and one that names them by an
// opaque build id both work, as long as both sides agree.
func NewStore(docs map[string]string) *Store {
	cp := make(map[string]string, len(docs))
	for id, text := range docs {
		cp[id] = text
	}
	return &Store{docs: cp}
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
