package veloxfx

import (
	"bytes"
	"fmt"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/velox/contrib/graphql/gqlrelay"
)

// scalars binds the two custom scalars velox's SDL declares. gqlc maps each
// to a Go type but cannot know how it marshals, and says so while generating.
var scalars = graphql.Options(
	graphql.Scalar("Time", marshalTime, unmarshalTime),
	// Cursor is declared whether or not a connection uses it. It already
	// speaks gqlgen's Marshaler contract, so this adapts that rather than
	// re-encoding it.
	graphql.Scalar("Cursor", marshalCursor, unmarshalCursor),
)

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

func marshalCursor(w *graphql.Writer, c gqlrelay.Cursor) error {
	var buf bytes.Buffer
	c.MarshalGQL(&buf)
	w.Raw(buf.Bytes())
	return nil
}

func unmarshalCursor(v any) (gqlrelay.Cursor, error) {
	var c gqlrelay.Cursor
	err := c.UnmarshalGQL(v)
	return c, err
}
