package gqlfiber

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/httpreq"
)

// Media types negotiated by the handler.
const (
	MediaTypeGraphQLResponse = httpreq.MediaTypeGraphQLResponse
	MediaTypeJSON            = httpreq.MediaTypeJSON
)

// handler serves GraphQL requests over Fiber.
type handler struct {
	exec *graphql.Executor
	*config
}

// New creates a Fiber handler executing requests with exec. It serves GET and
// POST; register it on both, or with App.All so that a rejected method is
// answered by this handler rather than by Fiber's router.
func New(exec *graphql.Executor, opts ...Option) fiber.Handler {
	h := &handler{exec: exec, config: newConfig(opts...)}
	return h.serve
}

// serve mirrors gqlhttp.Handler.ServeHTTP: the same checks in the same order,
// producing the same statuses and the same messages. It always returns nil,
// because the response is already written -- handing Fiber an error would let
// the application's error handler replace a GraphQL envelope with its own.
func (h *handler) serve(c fiber.Ctx) error {
	mediaType, ok := httpreq.Negotiate(c.Get("Accept"))
	if !ok {
		h.writeError(c, mediaType, http.StatusNotAcceptable, "Accept header does not allow %s or %s.", MediaTypeGraphQLResponse, MediaTypeJSON)
		return nil
	}

	method := c.Method()
	if method != http.MethodGet && method != http.MethodPost {
		c.Set("Allow", "GET, POST")
		h.writeError(c, mediaType, http.StatusMethodNotAllowed, "Method %s is not allowed; use GET or POST.", method)
		return nil
	}

	src := newSource(c)

	if h.csrf && httpreq.Forgeable(src, h.csrfHeaders) {
		h.writeError(c, mediaType, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return nil
	}

	var (
		reqs  []*graphql.Request
		batch bool
	)
	switch method {
	case http.MethodGet:
		req, err := httpreq.ParseGET(src, h.apq != nil)
		if err != nil {
			h.writeError(c, mediaType, http.StatusBadRequest, "%v", err)
			return nil
		}
		if resp := h.resolvePersisted(req); resp != nil {
			h.writeGraphQLError(c, mediaType, resp)
			return nil
		}
		if req.Query == "" {
			h.writeError(c, mediaType, http.StatusBadRequest, "%v", httpreq.ErrMissingQueryParam)
			return nil
		}
		kind, kerr := h.exec.OperationKind(req.Query, req.OperationName)
		if kerr == nil && kind != ast.Query {
			h.writeError(c, mediaType, http.StatusMethodNotAllowed, "%s operations (mutations are not allowed over GET); use POST.", strings.ToLower(string(kind)))
			return nil
		}
		reqs = []*graphql.Request{req}
	case http.MethodPost:
		var status int
		var err error
		reqs, batch, status, err = h.parsePOST(src)
		if err != nil {
			h.writeError(c, mediaType, status, "%v", err)
			return nil
		}
	}

	ctx, cancel := requestContext(c)
	defer cancel()

	if !batch {
		resp := h.execute(ctx, reqs[0])
		status := http.StatusOK
		if mediaType == MediaTypeGraphQLResponse && resp.HasRequestErrors() {
			status = http.StatusBadRequest
		}
		h.writeResponse(c, mediaType, status, func(out io.Writer) error {
			_, err := resp.WriteTo(out)
			return err
		})
		resp.Release()
		return nil
	}

	// Batches are always 200: each entry carries its own errors.
	h.writeResponse(c, mediaType, http.StatusOK, func(out io.Writer) error {
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
	return nil
}

// execute resolves a persisted query, if enabled, and runs the request.
func (h *handler) execute(ctx context.Context, req *graphql.Request) *graphql.Response {
	if resp := h.resolvePersisted(req); resp != nil {
		return resp
	}
	if req.Query == "" {
		return &graphql.Response{Errors: []*graphql.Error{graphql.Errorf("%v", httpreq.ErrMissingQuery)}}
	}
	return h.exec.Execute(ctx, req)
}

func (h *handler) resolvePersisted(req *graphql.Request) *graphql.Response {
	if h.apq == nil {
		return nil
	}
	return apq.Resolve(h.apq, req)
}

// writeGraphQLError sends a response carrying only errors, with the status the
// negotiated media type calls for. A client sending application/json gets 200,
// which is what a persisted-query client expects before it retries with the
// query text.
func (h *handler) writeGraphQLError(c fiber.Ctx, mediaType string, resp *graphql.Response) {
	status := http.StatusOK
	if mediaType == MediaTypeGraphQLResponse && resp.HasRequestErrors() {
		status = http.StatusBadRequest
	}
	h.writeResponse(c, mediaType, status, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
}

// parsePOST reads and decodes the body. The returned status applies when err
// is non-nil.
func (h *handler) parsePOST(src httpreq.Source) (reqs []*graphql.Request, batch bool, status int, err error) {
	if err := httpreq.RequireJSONBody(src); err != nil {
		return nil, false, http.StatusUnsupportedMediaType, err
	}

	body, status, err := httpreq.ReadBody(src, h.maxBody)
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

// writeResponse writes straight into the fasthttp response buffer: fiber.Ctx
// is an io.Writer, so the JSON never passes through an intermediate buffer.
func (h *handler) writeResponse(c fiber.Ctx, mediaType string, status int, write func(io.Writer) error) {
	c.Set("Content-Type", mediaType+"; charset=utf-8")
	c.Status(status)
	if err := write(c); err != nil {
		h.logger.Warn("gqlfiber: writing response", "error", err)
	}
}

// writeError sends a transport-level error as a GraphQL response envelope.
func (h *handler) writeError(c fiber.Ctx, mediaType string, status int, format string, args ...any) {
	resp := &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}}
	h.writeResponse(c, mediaType, status, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
}
