// Package gqlhttp serves GraphQL over HTTP following the GraphQL over HTTP
// specification: GET and POST requests, content negotiation between
// application/graphql-response+json and application/json, and CSRF
// prevention for requests browsers can send without a preflight.
package gqlhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/httpreq"
)

// Media types negotiated by the handler.
const (
	MediaTypeGraphQLResponse = "application/graphql-response+json"
	MediaTypeJSON            = httpreq.MediaTypeJSON
)

// DefaultCSRFHeaders are the headers whose presence marks a request as one
// that required a CORS preflight. Any of them satisfies the CSRF check.
var DefaultCSRFHeaders = httpreq.DefaultCSRFHeaders

// Handler serves GraphQL requests over HTTP.
type Handler struct {
	exec        *graphql.Executor
	maxBody     int64
	batchMax    int
	csrf        bool
	csrfHeaders []string
	apq         apq.Cache
	logger      *slog.Logger
}

// Option configures a Handler.
type Option func(*Handler)

// WithMaxBodyBytes limits the size of request bodies. The default is 1 MiB.
func WithMaxBodyBytes(n int64) Option { return func(h *Handler) { h.maxBody = n } }

// WithBatching accepts JSON arrays of requests with at most max entries,
// executed sequentially. Batching is disabled by default.
func WithBatching(maxEntries int) Option { return func(h *Handler) { h.batchMax = maxEntries } }

// WithCSRFPrevention controls the check that rejects requests a browser could
// send cross-origin without a CORS preflight. When enabled (the default),
// such requests must carry one of the given headers, or DefaultCSRFHeaders
// when none are given.
func WithCSRFPrevention(enabled bool, headers ...string) Option {
	return func(h *Handler) {
		h.csrf = enabled
		if len(headers) > 0 {
			h.csrfHeaders = headers
		}
	}
}

// WithPersistedQueries enables automatic persisted queries backed by cache,
// for example apq.NewCache(1000). Disabled by default.
func WithPersistedQueries(cache apq.Cache) Option { return func(h *Handler) { h.apq = cache } }

// WithLogger sets the logger for transport-level failures such as write
// errors. The default is slog.Default.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.logger = l } }

// New creates a handler executing requests with exec.
func New(exec *graphql.Executor, opts ...Option) *Handler {
	h := &Handler{
		exec:        exec,
		maxBody:     1 << 20,
		csrf:        true,
		csrfHeaders: DefaultCSRFHeaders,
	}
	for _, o := range opts {
		o(h)
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaType, ok := negotiate(r.Header.Get("Accept"))
	if !ok {
		h.writeError(w, mediaType, http.StatusNotAcceptable, "Accept header does not allow %s or %s.", MediaTypeGraphQLResponse, MediaTypeJSON)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.writeError(w, mediaType, http.StatusMethodNotAllowed, "Method %s is not allowed; use GET or POST.", r.Method)
		return
	}

	if h.csrf && httpreq.Forgeable(r, h.csrfHeaders) {
		h.writeError(w, mediaType, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return
	}

	var (
		reqs  []*graphql.Request
		batch bool
	)
	switch r.Method {
	case http.MethodGet:
		req, err := httpreq.ParseGET(r, h.apq != nil)
		if err != nil {
			h.writeError(w, mediaType, http.StatusBadRequest, "%v", err)
			return
		}
		if resp := h.resolvePersisted(req); resp != nil {
			h.writeGraphQLError(w, mediaType, resp)
			return
		}
		if req.Query == "" {
			h.writeError(w, mediaType, http.StatusBadRequest, "%v", httpreq.ErrMissingQueryParam)
			return
		}
		kind, kerr := h.exec.OperationKind(req.Query, req.OperationName)
		if kerr == nil && kind != ast.Query {
			h.writeError(w, mediaType, http.StatusMethodNotAllowed, "%s operations (mutations are not allowed over GET); use POST.", strings.ToLower(string(kind)))
			return
		}
		reqs = []*graphql.Request{req}
	case http.MethodPost:
		var status int
		var err error
		reqs, batch, status, err = h.parsePOST(w, r)
		if err != nil {
			h.writeError(w, mediaType, status, "%v", err)
			return
		}
	}

	ctx := r.Context()
	if !batch {
		resp := h.execute(ctx, reqs[0])
		status := http.StatusOK
		if mediaType == MediaTypeGraphQLResponse && resp.HasRequestErrors() {
			status = http.StatusBadRequest
		}
		h.writeResponse(w, mediaType, status, func(out io.Writer) error {
			_, err := resp.WriteTo(out)
			return err
		})
		resp.Release()
		return
	}

	// Batches are always 200: each entry carries its own errors.
	h.writeResponse(w, mediaType, http.StatusOK, func(out io.Writer) error {
		if _, err := io.WriteString(out, "["); err != nil {
			return err
		}
		for i, req := range reqs {
			if i > 0 {
				if _, err := io.WriteString(out, ","); err != nil {
					return err
				}
			}
			resp := h.execute(ctx, req)
			_, err := resp.WriteTo(out)
			resp.Release()
			if err != nil {
				return err
			}
		}
		_, err := io.WriteString(out, "]")
		return err
	})
}

// execute resolves a persisted query, if enabled, and runs the request.
func (h *Handler) execute(ctx context.Context, req *graphql.Request) *graphql.Response {
	if resp := h.resolvePersisted(req); resp != nil {
		return resp
	}
	if req.Query == "" {
		return &graphql.Response{Errors: []*graphql.Error{graphql.Errorf("%v", httpreq.ErrMissingQuery)}}
	}
	return h.exec.Execute(ctx, req)
}

func (h *Handler) resolvePersisted(req *graphql.Request) *graphql.Response {
	if h.apq == nil {
		return nil
	}
	return apq.Resolve(h.apq, req)
}

// writeGraphQLError sends a response carrying only errors, with the status the
// negotiated media type calls for. A client sending application/json gets 200,
// which is what a persisted-query client expects before it retries with the
// query text.
func (h *Handler) writeGraphQLError(w http.ResponseWriter, mediaType string, resp *graphql.Response) {
	status := http.StatusOK
	if mediaType == MediaTypeGraphQLResponse && resp.HasRequestErrors() {
		status = http.StatusBadRequest
	}
	h.writeResponse(w, mediaType, status, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
}

// parsePOST reads and decodes the body. The returned status applies when err
// is non-nil.
func (h *Handler) parsePOST(w http.ResponseWriter, r *http.Request) (reqs []*graphql.Request, batch bool, status int, err error) {
	if err := httpreq.RequireJSONBody(r); err != nil {
		return nil, false, http.StatusUnsupportedMediaType, err
	}

	body, status, err := httpreq.ReadBody(w, r, h.maxBody)
	if err != nil {
		return nil, false, status, err
	}

	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if h.batchMax <= 0 {
			return nil, false, http.StatusBadRequest, errors.New("batching is not enabled on this server.")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, false, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err)
		}
		if len(items) > h.batchMax {
			return nil, false, http.StatusBadRequest, fmt.Errorf("batch of %d exceeds the limit of %d.", len(items), h.batchMax)
		}
		reqs = make([]*graphql.Request, 0, len(items))
		for i, item := range items {
			req, err := httpreq.Decode(item, h.apq != nil)
			if err != nil {
				return nil, false, http.StatusBadRequest, fmt.Errorf("batch entry %d: %w", i, err)
			}
			reqs = append(reqs, req)
		}
		return reqs, true, 0, nil
	}

	req, err := httpreq.Decode(trimmed, h.apq != nil)
	if err != nil {
		return nil, false, http.StatusBadRequest, err
	}
	return []*graphql.Request{req}, false, 0, nil
}

// negotiate chooses the response media type from an Accept header. The
// second result is false when the client accepts neither supported type.
func negotiate(accept string) (string, bool) {
	if strings.TrimSpace(accept) == "" {
		return MediaTypeGraphQLResponse, true
	}
	best, bestQ := "", -1.0
	consider := func(mt string, q float64) {
		// Ties favour the specification's preferred type.
		if q > bestQ || (q == bestQ && mt == MediaTypeGraphQLResponse) {
			best, bestQ = mt, q
		}
	}
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			if parsed, err := strconv.ParseFloat(qs, 64); err == nil {
				q = parsed
			}
		}
		if q <= 0 {
			continue
		}
		switch mt {
		case MediaTypeGraphQLResponse, MediaTypeJSON:
			consider(mt, q)
		case "*/*", "application/*":
			// Wildcards match both; the preferred type wins the tie, but an
			// explicit application/json still outranks a wildcard at equal q.
			if q > bestQ {
				best, bestQ = MediaTypeGraphQLResponse, q
			}
		}
	}
	if best == "" {
		return MediaTypeGraphQLResponse, false
	}
	return best, true
}

func (h *Handler) writeResponse(w http.ResponseWriter, mediaType string, status int, write func(io.Writer) error) {
	w.Header().Set("Content-Type", mediaType+"; charset=utf-8")
	w.WriteHeader(status)
	if err := write(w); err != nil {
		h.logger.Warn("gqlhttp: writing response", "error", err)
	}
}

// writeError sends a transport-level error as a GraphQL response envelope.
func (h *Handler) writeError(w http.ResponseWriter, mediaType string, status int, format string, args ...any) {
	resp := &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}}
	h.writeResponse(w, mediaType, status, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
}
