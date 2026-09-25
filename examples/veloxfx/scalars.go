package veloxfx

import (
	"fmt"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// scalars binds Time, the one custom scalar gqlc cannot: time.Time does not
// encode itself the GraphQL way, so its format is the application's choice.
// velox's Cursor does (MarshalGQL, UnmarshalGQL), and gqlc binds it.
var scalars = graphql.Scalar("Time", marshalTime, unmarshalTime)

func marshalTime(w *graphql.Writer, t time.Time) error {
	w.String(t.UTC().Format(time.RFC3339Nano))
	return nil
}

func unmarshalTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string, got %T", v)
	}
	return time.Parse(time.RFC3339Nano, s)
}
