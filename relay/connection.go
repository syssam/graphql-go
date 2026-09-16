package relay

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// cursorPrefix is graphql-relay-js's, kept so a cursor issued by either
// implementation means the same thing to the other.
const cursorPrefix = "arrayconnection:"

// PageInfo is the Relay PageInfo type. StartCursor and EndCursor are nil on
// an empty page, which is why they are pointers: the SDL declares them
// nullable and an empty string is not the same answer as no answer.
type PageInfo struct {
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     *string
	EndCursor       *string
}

// Edge pairs a node with its cursor.
type Edge[T any] struct {
	Node   T
	Cursor string
}

// Connection is one page of edges plus the information needed to ask for
// the next.
type Connection[T any] struct {
	Edges    []Edge[T]
	PageInfo PageInfo
}

// Args are the four Relay pagination arguments. All are nullable: absent
// and zero mean different things, so first: 0 asks for no edges while no
// first at all asks for every remaining one.
type Args struct {
	First  *int
	Last   *int
	After  *string
	Before *string
}

// OffsetCursor is the cursor for the nth element of a list, base64 of
// "arrayconnection:n" — the encoding graphql-relay-js uses.
func OffsetCursor(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(offset)))
}

// cursorOffset reverses OffsetCursor, reporting whether it could.
func cursorOffset(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	rest, found := strings.CutPrefix(string(raw), cursorPrefix)
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// offsetOr returns the cursor's offset, or def when it is absent or not one
// this package issued. An unreadable cursor is not an error: the
// specification's ApplyCursorsToEdges ignores a cursor that names no edge.
func offsetOr(cursor *string, def int) int {
	if cursor == nil {
		return def
	}
	if n, ok := cursorOffset(*cursor); ok {
		return n
	}
	return def
}

// FromSlice paginates an in-memory slice, implementing the specification's
// ApplyCursorsToEdges and EdgesToReturn.
//
// It has to hold the whole list to do it. That is fine for a bounded set and
// wrong for anything a datastore should be slicing; use FromPage for those.
func FromSlice[T any](items []T, args Args) (Connection[T], error) {
	if args.First != nil && *args.First < 0 {
		return Connection[T]{}, fmt.Errorf("%w: first is %d", ErrNegativeCount, *args.First)
	}
	if args.Last != nil && *args.Last < 0 {
		return Connection[T]{}, fmt.Errorf("%w: last is %d", ErrNegativeCount, *args.Last)
	}

	length := len(items)
	afterOffset := offsetOr(args.After, -1)
	beforeOffset := offsetOr(args.Before, length)

	start := max(afterOffset+1, 0)
	end := min(beforeOffset, length)
	if args.First != nil {
		end = min(end, start+*args.First)
	}
	if args.Last != nil {
		start = max(start, end-*args.Last)
	}
	if start > end {
		start = end
	}

	lower, upper := 0, length
	if args.After != nil {
		lower = afterOffset + 1
	}
	if args.Before != nil {
		upper = beforeOffset
	}

	// The edges slice is made, never left nil: the SDL says [Edge!]! and a nil
	// slice would be written as null rather than an empty page.
	conn := Connection[T]{
		Edges: make([]Edge[T], 0, end-start),
		PageInfo: PageInfo{
			HasNextPage:     args.First != nil && end < upper,
			HasPreviousPage: args.Last != nil && start > lower,
		},
	}
	for i := start; i < end; i++ {
		conn.Edges = append(conn.Edges, Edge[T]{Node: items[i], Cursor: OffsetCursor(i)})
	}
	setBounds(&conn)
	return conn, nil
}

// FromPage builds a connection from a page the datastore already sliced.
// The caller supplies each element's cursor and the two boundary flags,
// because only the query that produced the page knows them.
func FromPage[T any](items []T, cursor func(T) string, hasNext, hasPrev bool) Connection[T] {
	conn := Connection[T]{
		Edges:    make([]Edge[T], 0, len(items)),
		PageInfo: PageInfo{HasNextPage: hasNext, HasPreviousPage: hasPrev},
	}
	for _, it := range items {
		conn.Edges = append(conn.Edges, Edge[T]{Node: it, Cursor: cursor(it)})
	}
	setBounds(&conn)
	return conn
}

// ErrNegativeCount is returned for first or last below zero, which the
// specification calls out as an error rather than an empty page.
var ErrNegativeCount = errors.New("relay: pagination count must not be negative")

func setBounds[T any](c *Connection[T]) {
	if len(c.Edges) == 0 {
		return
	}
	c.PageInfo.StartCursor = &c.Edges[0].Cursor
	c.PageInfo.EndCursor = &c.Edges[len(c.Edges)-1].Cursor
}
