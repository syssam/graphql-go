package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Error codes reported in the "code" entry of an error's extensions. They
// follow the conventions used by the wider GraphQL ecosystem so that generic
// clients can act on them.
const (
	CodeInternal            = "INTERNAL_SERVER_ERROR"
	CodeParseFailed         = "GRAPHQL_PARSE_FAILED"
	CodeValidationFailed    = "GRAPHQL_VALIDATION_FAILED"
	CodeBadUserInput        = "BAD_USER_INPUT"
	CodeOperationResolution = "OPERATION_RESOLUTION_FAILURE"
	CodeRequestCancelled    = "REQUEST_CANCELLED"
	// CodeTooComplex is returned when static field count or query cost
	// exceeds the executor limit (GitHub / Shopify style).
	CodeTooComplex = "COMPLEXITY_LIMIT_EXCEEDED"
	// CodeMaxDepth is returned when the selection nesting exceeds WithMaxDepth.
	CodeMaxDepth = "MAX_DEPTH_EXCEEDED"
	// CodeForbidden is returned when an Authorizer denies a site.
	CodeForbidden = "FORBIDDEN"
	// CodeResponseTooLarge is returned when a response's data exceeds
	// WithMaxResponseBytes.
	CodeResponseTooLarge = "RESPONSE_TOO_LARGE"
	// CodeOperationTimeout is returned for work cut short by
	// WithOperationTimeout. When a deadline the caller set ends execution,
	// the executor reports CodeRequestCancelled instead.
	CodeOperationTimeout = "OPERATION_TIMEOUT"
	// CodeErrorLimitExceeded marks the one error that stands in for those
	// omitted past WithMaxErrors.
	CodeErrorLimitExceeded = "ERROR_LIMIT_EXCEEDED"
)

// Location is a line/column position in the request document.
type Location struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// PathElem is one segment of a response path: either a field key or a list
// index.
type PathElem struct {
	Key     string
	Index   int
	IsIndex bool
}

// Path locates a value in the response data, as defined by the GraphQL
// specification's error format.
type Path []PathElem

// String renders the path in dotted notation, for example user.posts[2].title.
func (p Path) String() string {
	var sb strings.Builder
	for i, e := range p {
		if e.IsIndex {
			sb.WriteByte('[')
			sb.WriteString(strconv.Itoa(e.Index))
			sb.WriteByte(']')
			continue
		}
		if i > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(e.Key)
	}
	return sb.String()
}

// MarshalJSON renders the path as a JSON array of strings and integers.
func (p Path) MarshalJSON() ([]byte, error) {
	return p.appendJSON(nil), nil
}

func (p Path) appendJSON(dst []byte) []byte {
	dst = append(dst, '[')
	for i, e := range p {
		if i > 0 {
			dst = append(dst, ',')
		}
		if e.IsIndex {
			dst = strconv.AppendInt(dst, int64(e.Index), 10)
		} else {
			dst = jsonw.AppendString(dst, e.Key)
		}
	}
	return append(dst, ']')
}

// Error is a GraphQL error as it appears in the "errors" array of a
// response. Err carries the underlying cause for logging and is never
// serialized.
type Error struct {
	Message    string
	Locations  []Location
	Path       Path
	Extensions map[string]any
	Err        error

	// ord is the document-order sort key for a field error: one element per
	// path segment (see pathNode.materializeOrder). It is unexported because
	// it is an execution detail, and nil on an error that did not come from a
	// field -- those keep their arrival order, after the sorted ones.
	ord []int32
}

// Errorf formats a new Error.
func Errorf(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if len(e.Path) == 0 {
		return e.Message
	}
	return e.Path.String() + ": " + e.Message
}

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// WithCode sets the "code" extension and returns e.
func (e *Error) WithCode(code string) *Error {
	return e.WithExtension("code", code)
}

// WithExtension sets one extension entry and returns e.
//
// v must marshal with encoding/json. It is not checked here, and the failure
// surfaces late and badly: Response.WriteTo and MarshalJSON return an error
// for the whole envelope, and a transport has already written its status
// header by then, so the client receives an empty body with no explanation
// while the server logs a warning. One decorative extension can therefore
// cost the error it was decorating. Marshal it yourself if the value is
// anything but a string, number, bool, slice or map of those.
func (e *Error) WithExtension(key string, v any) *Error {
	if e.Extensions == nil {
		e.Extensions = make(map[string]any, 1)
	}
	e.Extensions[key] = v
	return e
}

// WithPath sets the response path and returns e.
func (e *Error) WithPath(p Path) *Error {
	e.Path = p
	return e
}

// sortErrorsByDocumentOrder puts field errors in the order their fields appear
// in the document, which is the order graphql-js, graphql-http, Apollo Server
// and graphql-yoga all report. Fields here finish in whatever order their
// resolvers return, so without this the same query answers the same errors in a
// different order run to run, while data -- written by walking the plan -- is
// stable. Build errors are already sorted for the same reason.
//
// The sort is stable and errors carrying no key keep their arrival order after
// the rest: that is the error-limit notice, which belongs last, and request
// errors, which never accompany field errors.
func sortErrorsByDocumentOrder(errs []*Error) {
	if len(errs) < 2 {
		return
	}
	slices.SortStableFunc(errs, func(a, b *Error) int {
		switch {
		case a.ord == nil && b.ord == nil:
			return 0
		case a.ord == nil:
			return 1
		case b.ord == nil:
			return -1
		}
		return slices.Compare(a.ord, b.ord)
	})
}

// clone returns a shallow copy with its own Extensions map so that callers
// can annotate the error without mutating a shared value.
func (e *Error) clone() *Error {
	c := *e
	if e.Extensions != nil {
		c.Extensions = maps.Clone(e.Extensions)
	}
	// Path and Locations are copied too: built by append they can have spare
	// capacity, and an append on one copy would write into the other's array.
	c.Path = slices.Clone(e.Path)
	c.Locations = slices.Clone(e.Locations)
	return &c
}

// MarshalJSON renders the error in the specification's format, emitting
// message, locations, path and extensions in that order and omitting empty
// members.
func (e *Error) MarshalJSON() ([]byte, error) {
	return e.appendJSON(nil)
}

func (e *Error) appendJSON(dst []byte) ([]byte, error) {
	dst = append(dst, `{"message":`...)
	dst = jsonw.AppendString(dst, e.Message)
	if len(e.Locations) > 0 {
		dst = append(dst, `,"locations":[`...)
		for i, l := range e.Locations {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, `{"line":`...)
			dst = strconv.AppendInt(dst, int64(l.Line), 10)
			dst = append(dst, `,"column":`...)
			dst = strconv.AppendInt(dst, int64(l.Column), 10)
			dst = append(dst, '}')
		}
		dst = append(dst, ']')
	}
	if len(e.Path) > 0 {
		dst = append(dst, `,"path":`...)
		dst = e.Path.appendJSON(dst)
	}
	if len(e.Extensions) > 0 {
		ext, err := json.Marshal(e.Extensions)
		if err != nil {
			return nil, fmt.Errorf("graphql: marshal error extensions: %w", err)
		}
		dst = append(dst, `,"extensions":`...)
		dst = append(dst, ext...)
	}
	return append(dst, '}'), nil
}

// ExtensionsProvider may be implemented by error types to contribute entries
// to the "extensions" member of the presented GraphQL error.
type ExtensionsProvider interface {
	GraphQLExtensions() map[string]any
}

// ErrorPresenter converts any error raised during a request into the Error
// that is sent to the client. Implementations typically mask internal
// messages and attach codes.
type ErrorPresenter func(ctx context.Context, err error) *Error

// DefaultErrorPresenter returns the first *Error or *gqlerror.Error found in
// err's chain (copied, so annotating it is safe), or wraps err verbatim.
// Extensions contributed by an ExtensionsProvider in the chain are merged in.
func DefaultErrorPresenter(_ context.Context, err error) *Error {
	var out *Error
	var gerr *Error
	var perr *gqlerror.Error
	switch {
	case errors.As(err, &gerr):
		out = gerr.clone()
		//nolint:errorlint // identity, not equality: did As unwrap anything?
		if out.Err == nil && gerr != err {
			out.Err = err
		}
	case errors.As(err, &perr):
		out = fromGQLError(perr)
	default:
		out = &Error{Message: err.Error(), Err: err}
	}
	var ep ExtensionsProvider
	if errors.As(err, &ep) {
		for k, v := range ep.GraphQLExtensions() {
			if _, exists := out.Extensions[k]; !exists {
				out = out.WithExtension(k, v)
			}
		}
	}
	return out
}

// fromGQLError converts a parser or validator error.
func fromGQLError(e *gqlerror.Error) *Error {
	out := &Error{Message: e.Message, Err: e}
	for _, l := range e.Locations {
		out.Locations = append(out.Locations, Location{Line: l.Line, Column: l.Column})
	}
	for _, p := range e.Path {
		switch v := p.(type) {
		case ast.PathName:
			out.Path = append(out.Path, PathElem{Key: string(v)})
		case ast.PathIndex:
			out.Path = append(out.Path, PathElem{Index: int(v), IsIndex: true})
		default:
			out.Path = append(out.Path, PathElem{Key: fmt.Sprint(v)})
		}
	}
	if len(e.Extensions) > 0 {
		out.Extensions = maps.Clone(e.Extensions)
	}
	return out
}
