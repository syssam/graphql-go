package relay_test

import (
	"encoding/base64"
	"testing"

	"github.com/syssam/graphql-go/relay"
)

var letters = []string{"a", "b", "c", "d", "e"}

func ptr[T any](v T) *T { return &v }

func nodes[T any](c relay.Connection[T]) []T {
	out := make([]T, 0, len(c.Edges))
	for _, e := range c.Edges {
		out = append(out, e.Node)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The cases are the Relay specification's EdgesToReturn worked through by
// hand over a five-element list.
func TestFromSliceFollowsTheSpecAlgorithm(t *testing.T) {
	for _, tc := range []struct {
		name string
		args relay.Args
		want []string
		next bool
		prev bool
	}{
		{name: "no arguments returns everything", want: letters},
		{name: "first truncates from the start", args: relay.Args{First: ptr(2)}, want: []string{"a", "b"}, next: true},
		{name: "after skips past the cursor", args: relay.Args{After: ptr(relay.OffsetCursor(1)), First: ptr(2)}, want: []string{"c", "d"}, next: true},
		{name: "last truncates from the end", args: relay.Args{Last: ptr(2)}, want: []string{"d", "e"}, prev: true},
		{name: "before stops at the cursor", args: relay.Args{Before: ptr(relay.OffsetCursor(3)), Last: ptr(2)}, want: []string{"b", "c"}, prev: true},
		{name: "first zero returns nothing but reports more", args: relay.Args{First: ptr(0)}, want: nil, next: true},
		{name: "after the last element is empty", args: relay.Args{After: ptr(relay.OffsetCursor(4))}, want: nil},
		{name: "first past the end has no next page", args: relay.Args{First: ptr(99)}, want: letters},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := relay.FromSlice(letters, tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := nodes(c); !eq(got, tc.want) {
				t.Fatalf("nodes = %v, want %v", got, tc.want)
			}
			if c.PageInfo.HasNextPage != tc.next {
				t.Fatalf("hasNextPage = %v, want %v", c.PageInfo.HasNextPage, tc.next)
			}
			if c.PageInfo.HasPreviousPage != tc.prev {
				t.Fatalf("hasPreviousPage = %v, want %v", c.PageInfo.HasPreviousPage, tc.prev)
			}
		})
	}
}

func TestFromSliceCursorsBoundTheEdges(t *testing.T) {
	c, err := relay.FromSlice(letters, relay.Args{After: ptr(relay.OffsetCursor(0)), First: ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if c.PageInfo.StartCursor == nil || *c.PageInfo.StartCursor != relay.OffsetCursor(1) {
		t.Fatalf("startCursor = %v", c.PageInfo.StartCursor)
	}
	if c.PageInfo.EndCursor == nil || *c.PageInfo.EndCursor != relay.OffsetCursor(2) {
		t.Fatalf("endCursor = %v", c.PageInfo.EndCursor)
	}
	if c.Edges[0].Cursor != relay.OffsetCursor(1) {
		t.Fatalf("edge cursor = %q", c.Edges[0].Cursor)
	}
}

// An empty page has no cursors at all rather than empty-string ones.
func TestFromSliceOnAnEmptyListHasNoCursors(t *testing.T) {
	c, err := relay.FromSlice([]string{}, relay.Args{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Edges) != 0 || c.PageInfo.StartCursor != nil || c.PageInfo.EndCursor != nil {
		t.Fatalf("connection = %+v", c)
	}
}

func TestFromSliceRejectsNegativeCounts(t *testing.T) {
	if _, err := relay.FromSlice(letters, relay.Args{First: ptr(-1)}); err == nil {
		t.Fatal("first: -1 was accepted")
	}
	if _, err := relay.FromSlice(letters, relay.Args{Last: ptr(-1)}); err == nil {
		t.Fatal("last: -1 was accepted")
	}
}

// FromPage is for the case the datastore already did the slicing, so the
// caller supplies the cursors and the boundary flags.
func TestFromPageUsesTheCallersCursors(t *testing.T) {
	c := relay.FromPage([]string{"c", "d"}, func(s string) string { return "cur-" + s }, true, true)
	if got := nodes(c); !eq(got, []string{"c", "d"}) {
		t.Fatalf("nodes = %v", got)
	}
	if c.Edges[0].Cursor != "cur-c" || c.Edges[1].Cursor != "cur-d" {
		t.Fatalf("cursors = %q %q", c.Edges[0].Cursor, c.Edges[1].Cursor)
	}
	if !c.PageInfo.HasNextPage || !c.PageInfo.HasPreviousPage {
		t.Fatalf("pageInfo = %+v", c.PageInfo)
	}
	if *c.PageInfo.StartCursor != "cur-c" || *c.PageInfo.EndCursor != "cur-d" {
		t.Fatalf("bounds = %v %v", c.PageInfo.StartCursor, c.PageInfo.EndCursor)
	}
}

// A cursor a client sends back is not necessarily one this package issued: it
// can come from a different connection, a truncated URL, an older encoding, or
// a client that made one up. The specification's ApplyCursorsToEdges ignores a
// cursor naming no edge, so every unreadable form has to fall back to the
// default rather than error or, worse, decode to some other page. Every other
// test in this file round-trips a cursor OffsetCursor produced, so none of the
// three ways decoding can fail was ever driven.
func TestAnUnreadableCursorIsIgnoredRatherThanMisread(t *testing.T) {
	letters := []string{"a", "b", "c", "d", "e"}
	full, err := relay.FromSlice(letters, relay.Args{First: ptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"not base64 at all!!",                                               // base64 decode fails
		base64.StdEncoding.EncodeToString([]byte("x:1")),                    // decodes, wrong prefix
		base64.StdEncoding.EncodeToString([]byte("arrayconnection:eleven")), // right prefix, not a number
		"",
	} {
		c, err := relay.FromSlice(letters, relay.Args{After: ptr(bad), First: ptr(2)})
		if err != nil {
			t.Errorf("after=%q returned an error: %v", bad, err)
			continue
		}
		if len(c.Edges) != len(full.Edges) {
			t.Errorf("after=%q returned %d edges, want the unpaged %d", bad, len(c.Edges), len(full.Edges))
			continue
		}
		for i := range c.Edges {
			if c.Edges[i].Node != full.Edges[i].Node || c.Edges[i].Cursor != full.Edges[i].Cursor {
				t.Errorf("after=%q edge %d = %v/%s, want %v/%s; an unreadable cursor moved the window",
					bad, i, c.Edges[i].Node, c.Edges[i].Cursor, full.Edges[i].Node, full.Edges[i].Cursor)
			}
		}
	}
}
