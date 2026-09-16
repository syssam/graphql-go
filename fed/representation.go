package fed

import (
	"encoding/json"

	graphql "github.com/syssam/graphql-go"
)

// Representation is one entry of the representations argument: the entity's
// __typename plus whatever its @key selects, and whatever any @requires on a
// field the router asked for named as well.
//
// Values arrive as they decode from JSON. A string field is a string and a
// boolean is a bool, but **a number is a json.Number**, not a float64,
// because the decoder keeps the text so that Int and Float coercion are both
// exact. So r["weight"].(float64) is the assertion that looks right and
// silently yields zero; use Float, Int or ID instead, which read both forms.
type Representation map[string]any

// Typename is the __typename the router sent, which names the entity type
// this representation stands for.
func (r Representation) Typename() string {
	s, _ := r["__typename"].(string)
	return s
}

// ID reads a key field declared ID, the commonest shape of all.
func (r Representation) ID(key string) (graphql.ID, bool) {
	switch v := r[key].(type) {
	case string:
		return graphql.ID(v), true
	case json.Number:
		return graphql.ID(v.String()), true
	}
	return "", false
}

// Int reads an integer field.
func (r Representation) Int(key string) (int64, bool) {
	switch v := r[key].(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int64:
		return v, true
	case float64:
		return int64(v), v == float64(int64(v))
	}
	return 0, false
}

// Float reads a floating-point field.
func (r Representation) Float(key string) (float64, bool) {
	switch v := r[key].(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int64:
		return float64(v), true
	}
	return 0, false
}
