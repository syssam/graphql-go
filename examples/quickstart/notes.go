package main

import (
	"context"
	_ "embed"
	"strconv"
	"sync"

	"github.com/syssam/graphql-go"
)

//go:embed schema.graphql
var sdl string

// Note is the Go shape bound to the Note GraphQL type.
type Note struct {
	ID    string
	Title string
	Body  string
}

// store is an in-memory note board standing in for a database. It is safe
// for concurrent use because query, mutation and subscription resolvers all
// run against it concurrently.
type store struct {
	mu     sync.RWMutex
	notes  []*Note
	nextID int

	created broker
}

func newStore() *store {
	return &store{
		notes:  []*Note{{ID: "1", Title: "Welcome", Body: "This is the first note."}},
		nextID: 2,
	}
}

func (s *store) Notes() []*Note {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Note, len(s.notes))
	copy(out, s.notes)
	return out
}

func (s *store) Note(id string) *Note {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.notes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (s *store) CreateNote(title, body string) *Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &Note{ID: strconv.Itoa(s.nextID), Title: title, Body: body}
	s.nextID++
	s.notes = append(s.notes, n)
	s.created.publish(n)
	return n
}

// NotesCreated returns a stream of notes created from now on.
func (s *store) NotesCreated(ctx context.Context) <-chan *Note {
	return s.created.subscribe(ctx)
}

// broker fans each created note out to every open subscription. It follows
// the same two rules as examples/blog/internal/repository/broker.go: a
// subscriber that is not keeping up is dropped rather than allowed to block
// a mutation (the send is non-blocking, and a full buffer loses the event),
// and a subscriber is removed when its context ends, which is how the
// transports report both an unsubscribe and a disconnect.
type broker struct {
	mu   sync.Mutex
	next int
	subs map[int]chan *Note
}

func (b *broker) subscribe(ctx context.Context) <-chan *Note {
	ch := make(chan *Note, 16)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[int]chan *Note)
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.remove(id)
	}()
	return ch
}

func (b *broker) remove(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Closing under the same lock publish holds is what makes a send to a
	// closed channel impossible.
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

func (b *broker) publish(n *Note) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- n:
		default:
		}
	}
}

type noteArgs struct{ ID graphql.ID }

type createNoteArgs struct {
	Title string
	Body  string
}

// newSchema binds the note board to the embedded SDL. Everything here is
// hand-written rather than generated -- see examples/blog and cmd/gqlc for
// the codegen path this example deliberately skips, to stay copyable as a
// single small package.
func newSchema(st *store) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[Note]("Note",
			graphql.Field("id", func(n *Note) graphql.ID { return graphql.ID(n.ID) }),
			graphql.Field("title", func(n *Note) string { return n.Title }),
			graphql.Field("body", func(n *Note) string { return n.Body }),
		),
		graphql.Args[noteArgs](),
		graphql.Args[createNoteArgs](),
		graphql.Query(
			graphql.Field("notes", func(graphql.Root) []*Note { return st.Notes() }),
			graphql.FieldArgs("note", func(_ graphql.Root, a noteArgs) *Note { return st.Note(string(a.ID)) }),
		),
		graphql.Mutation(
			graphql.FieldArgs("createNote", func(_ graphql.Root, a createNoteArgs) *Note {
				return st.CreateNote(a.Title, a.Body)
			}),
		),
		graphql.Subscription(
			graphql.Subscribe("noteCreated", func(ctx context.Context) (<-chan *Note, error) {
				return st.NotesCreated(ctx), nil
			}),
		),
	)
}
