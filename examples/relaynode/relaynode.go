// Package relaynode is the Relay server contract end to end: global object
// identification and cursor connections, with nothing hand-rolled.
//
// The relay package had no example before this one. Its own tests exercise
// every function, but a test shows one call at a time; what a reader needs is
// the three calls a connection takes sitting next to the SDL they bind to, and
// a Node resolver that has to dispatch on more than one type.
//
// What each piece is for:
//
//   - relay.IDField writes base64("Type:id") for the id field -- byte for byte
//     what graphql-relay-js and graphql-java produce, so a Relay client cannot
//     tell which server it is talking to.
//   - relay.Node binds Query.node, decoding the global id and handing the
//     type name and local id to one resolver.
//   - relay.Pagination once per schema, relay.Bind once per connection,
//     relay.FromSlice or relay.FromPage in the resolver.
//
// examples/blog also has a Node interface, hand-written and without this
// package. Both are legitimate: blog's shows that Query.node needs no engine
// support at all, since an unbound interface resolves from the dynamic Go
// type. This one shows what you get for not writing it yourself -- the global
// id encoding, the cursor arithmetic and the four-argument pagination
// contract, none of which are interesting to re-derive and all of which a
// Relay client will notice you got wrong.
package relaynode

import (
	"context"
	_ "embed"
	"fmt"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/relay"
)

//go:embed schema.graphql
var sdl string

// User owns repositories.
type User struct {
	ID   string
	Name string
}

// Repository belongs to a user.
type Repository struct {
	ID      string
	Name    string
	Stars   int
	OwnerID string
}

// Store is the in-memory data this example serves.
type Store struct {
	users        []*User
	repositories []*Repository
}

// NewStore returns the seeded store.
func NewStore() *Store {
	return &Store{
		users: []*User{
			{ID: "1", Name: "Ada Lovelace"},
			{ID: "2", Name: "Grace Hopper"},
		},
		repositories: []*Repository{
			{ID: "10", Name: "analytical-engine", Stars: 421, OwnerID: "1"},
			{ID: "11", Name: "note-g", Stars: 97, OwnerID: "1"},
			{ID: "12", Name: "cobol", Stars: 1863, OwnerID: "2"},
			{ID: "13", Name: "flow-matic", Stars: 64, OwnerID: "2"},
			{ID: "14", Name: "nanosecond", Stars: 12, OwnerID: "2"},
		},
	}
}

// User returns one user by local id, or nil.
func (s *Store) User(id string) *User {
	for _, u := range s.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// Repository returns one repository by local id, or nil.
func (s *Store) Repository(id string) *Repository {
	for _, r := range s.repositories {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Users returns every user.
func (s *Store) Users() []*User { return s.users }

// Repositories returns every repository.
func (s *Store) Repositories() []*Repository { return s.repositories }

// RepositoriesOf returns one user's repositories.
func (s *Store) RepositoriesOf(ownerID string) []*Repository {
	out := make([]*Repository, 0, 4)
	for _, r := range s.repositories {
		if r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out
}

// New builds the schema and returns it with the store behind it.
func New() (*graphql.Schema, *Store, error) {
	store := NewStore()
	s, err := NewSchema(store)
	return s, store, err
}

// NewSchema builds the schema over an existing store.
func NewSchema(store *Store) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(sdl),
		// Once per schema: PageInfo and the decoder for first/after/last/before.
		// Separate from Bind because Bind is generic over the node type and
		// these are not -- folding them together would register PageInfo once
		// per connection.
		relay.Pagination(),

		// Once per connection. The SDL types must already exist; this binds
		// Connection[T] and Edge[T] to them.
		relay.Bind[*User]("UserConnection", "UserEdge"),
		relay.Bind[*Repository]("RepositoryConnection", "RepositoryEdge"),

		graphql.Object[User]("User",
			// Not Field("id", ...): IDField encodes the global id. Returning
			// the local id here instead is the mistake that makes node(id:)
			// fail for every object, and it fails quietly, because the local
			// id is a perfectly good ID! as far as the schema is concerned.
			relay.IDField("User", func(u *User) string { return u.ID }),
			graphql.Field("name", func(u *User) string { return u.Name }),
			graphql.ResolveArgs("repositories", func(_ context.Context, u *User, a relay.Args) (*relay.Connection[*Repository], error) {
				c, err := relay.FromSlice(store.RepositoriesOf(u.ID), a)
				return &c, err
			}),
		),

		graphql.Object[Repository]("Repository",
			relay.IDField("Repository", func(r *Repository) string { return r.ID }),
			graphql.Field("name", func(r *Repository) string { return r.Name }),
			graphql.Field("stars", func(r *Repository) int { return r.Stars }),
			graphql.Resolve("owner", func(_ context.Context, r *Repository) (*User, error) {
				return store.User(r.OwnerID), nil
			}),
		),

		// Query.node. The global id is decoded before this runs, so the
		// resolver sees a type name and a local id and never touches base64.
		//
		// A type name it does not know is a nil, not an error: the id decoded
		// cleanly and named something this server does not serve, which is a
		// null Node and not a failure. An id that is not a global id at all
		// fails inside relay.Node before this is called.
		relay.Node(func(_ context.Context, typeName, id string) (any, error) {
			switch typeName {
			case "User":
				if u := store.User(id); u != nil {
					return u, nil
				}
			case "Repository":
				if r := store.Repository(id); r != nil {
					return r, nil
				}
			}
			return nil, nil
		}),

		graphql.Query(
			graphql.ResolveArgs("users", func(_ context.Context, _ graphql.Root, a relay.Args) (*relay.Connection[*User], error) {
				c, err := relay.FromSlice(store.Users(), a)
				return &c, err
			}),
			graphql.ResolveArgs("repositories", func(_ context.Context, _ graphql.Root, a relay.Args) (*relay.Connection[*Repository], error) {
				c, err := relay.FromSlice(store.Repositories(), a)
				return &c, err
			}),
		),
	)
}

// GlobalID is what a client would have received in a previous response and
// hands back to node(id:). It is here so the tests and the README can show the
// round trip without reaching into the relay package.
func GlobalID(typeName, localID string) graphql.ID {
	return relay.ToGlobalID(typeName, localID)
}

// Describe renders a global id the way a debugging tool would, which is the
// one time anyone should decode one by hand.
func Describe(gid graphql.ID) (string, error) {
	typeName, id, err := relay.FromGlobalID(gid)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s#%s", typeName, id), nil
}
