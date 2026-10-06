package howto

import (
	"context"
	"net/http"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const notesSDL = `
type Note { id: ID! text: String! replies: [Note!]! }
type Query { notes: [Note!]! note(id: ID!): Note! }
`

type Note struct {
	ID      string
	Text    string
	Replies []*Note
}

// A server is three values, each built from the one before, and each is a
// place a test can stand: the schema, the executor, the HTTP handler.

func NewNotesSchema(notes []*Note) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(notesSDL), notesBindings(notes)...)
}

// notesBindings is separate from NewNotesSchema so a test can build the same
// schema with one binding changed and watch the build refuse it.
func notesBindings(notes []*Note) []graphql.SchemaOption {
	return []graphql.SchemaOption{
		graphql.Object[Note]("Note",
			graphql.Field("id", func(n *Note) graphql.ID { return graphql.ID(n.ID) }),
			graphql.Field("text", func(n *Note) string { return n.Text }),
			graphql.Field("replies", func(n *Note) []*Note { return n.Replies }),
		),
		graphql.Query(
			graphql.Resolve("notes", func(context.Context, graphql.Root) ([]*Note, error) {
				return notes, nil
			}),
			graphql.ResolveArgs("note", func(_ context.Context, _ graphql.Root, a noteArgs) (*Note, error) {
				for _, n := range notes {
					if n.ID == string(a.ID) {
						return n, nil
					}
				}
				return nil, graphql.Errorf("no note with id %q", a.ID).WithCode("NOT_FOUND")
			}),
		),
	}
}

type noteArgs struct{ ID graphql.ID }

// NewNotesExecutor is where the limits live. Note.replies is a Note, so a
// client can nest a query as deep as it likes; nothing refuses that until a
// limit is set here.
func NewNotesExecutor(schema *graphql.Schema) *graphql.Executor {
	return graphql.NewExecutor(schema,
		graphql.WithMaxDepth(4),
		graphql.WithOperationTimeout(5*time.Second),
	)
}

func NewNotesHandler(exec *graphql.Executor) http.Handler {
	return gqlhttp.New(exec)
}
