// Package httpapi serves the REST control plane: the user API on the api
// host and the admin API on the admin host. Handlers decode the wire types
// in package api, call the domain services and encode the result; domain
// errors become statuses in one place, present.Error.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/auth"
	"kazari/internal/present"
	"kazari/internal/user"
)

// jsonLimit caps plain JSON bodies. Bodies that may carry inline files are
// capped at UserOptions.MaxBodyBytes instead.
const jsonLimit = 1 << 20

// ErrGrantNotFound is what a GrantStore returns (or wraps) for an unknown
// grant id, so DELETE /v1/grants/{id} answers 404.
var ErrGrantNotFound = errors.New("httpapi: grant not found")

// statusError is an error that carries its own HTTP status, for the few
// failures that are HTTP concerns rather than domain errors.
type statusError struct {
	status     int
	code       string
	msg        string
	retryAfter time.Duration // sent as Retry-After on 429
}

func (e *statusError) Error() string { return e.code + ": " + e.msg }

// invalid is a 422 invalid_argument with a message for the client.
func invalid(format string, args ...any) error {
	return artifact.Errorf(artifact.ErrInvalidArgument, format, args...)
}

// apiFunc is a handler that reports failure by returning an error; the
// wrapper writes it. p is the authenticated caller.
type apiFunc func(w http.ResponseWriter, r *http.Request, p auth.Principal) error

// base holds what every handler needs to answer.
type base struct {
	log   *slog.Logger
	links present.Links
}

func newBase(log *slog.Logger, links present.Links) base {
	if log == nil {
		log = slog.Default()
	}
	return base{log: log, links: links}
}

// serve adapts an apiFunc to http.Handler.
func (b *base) serve(fn apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		if err := fn(w, r, p); err != nil {
			b.writeError(w, r, err)
		}
	})
}

// writeJSON writes v as the JSON response body with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the single place an error becomes a response. 5xx errors
// are logged with their cause; the body only ever carries a generic message
// for them.
func (b *base) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, body, retry := errorResponse(err)
	if status >= http.StatusInternalServerError {
		b.log.ErrorContext(r.Context(), "httpapi: request failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	}
	if status == http.StatusTooManyRequests {
		secs := int64(math.Ceil(retry.Seconds()))
		w.Header().Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
	}
	writeJSON(w, status, api.ErrorResponse{Error: body})
}

func errorResponse(err error) (int, api.Error, time.Duration) {
	var se *statusError
	if errors.As(err, &se) {
		return se.status, api.Error{Code: se.code, Message: se.msg}, se.retryAfter
	}
	switch {
	case errors.Is(err, user.ErrInvalidIdentity):
		return http.StatusUnprocessableEntity, api.Error{Code: api.CodeInvalidArgument, Message: err.Error()}, 0
	case errors.Is(err, ErrGrantNotFound):
		return http.StatusNotFound, api.Error{Code: api.CodeNotFound, Message: "grant not found"}, 0
	}
	status, body := present.Error(err)
	return status, body, 0
}

// decodeJSON decodes the request body into v, rejecting unknown fields,
// trailing data and bodies over limit (413). An empty body is a 422 unless
// optional, in which case v is left as it is.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any, optional bool) error {
	body := http.MaxBytesReader(w, r.Body, limit)
	return decodeStrict(body, v, optional)
}

// decodeStrict is decodeJSON over any reader.
func decodeStrict(rd io.Reader, v any, optional bool) error {
	dec := json.NewDecoder(rd)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			if optional {
				return nil
			}
			return invalid("a JSON request body is required")
		}
		return bodyErr("malformed JSON body", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			if mbe := maxBytesErr(err); mbe != nil {
				return mbe
			}
		}
		return invalid("unexpected data after the JSON body")
	}
	return nil
}

// bodyErr keeps a body-size error (413) and turns anything else that went
// wrong while reading a request body into a 422 naming what was wrong.
func bodyErr(what string, err error) error {
	if mbe := maxBytesErr(err); mbe != nil {
		return mbe
	}
	return invalid("%s: %v", what, err)
}

func maxBytesErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return mbe
	}
	return nil
}

// queryBool parses an optional boolean query parameter.
func queryBool(r *http.Request, name string) (bool, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, invalid("%s must be true or false", name)
	}
	return b, nil
}

// queryInt parses an optional non-negative integer query parameter.
func queryInt(r *http.Request, name string) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, invalid("%s must be a non-negative integer", name)
	}
	return n, nil
}

// listQuery reads the artifact list filters shared by both APIs.
func listQuery(r *http.Request) (artifact.ListQuery, error) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		return artifact.ListQuery{}, err
	}
	lq := artifact.ListQuery{
		Mine:    q.Get("mine"),
		Stale:   q.Get("stale"),
		Deleted: q.Get("deleted"),
		Cursor:  q.Get("cursor"),
		Limit:   limit,
	}
	for k, vs := range q {
		key, ok := strings.CutPrefix(k, "metadata.")
		if !ok || len(vs) == 0 {
			continue
		}
		if lq.Metadata == nil {
			lq.Metadata = map[string]string{}
		}
		lq.Metadata[key] = vs[0]
	}
	return lq, nil
}

// jsonFallback serves mux, answering unmatched routes with JSON 404 and
// wrong methods with JSON 405 (keeping the mux's Allow header) instead of
// the mux's plain-text pages.
func jsonFallback(mux *http.ServeMux, b *base) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &probeWriter{header: http.Header{}}
		h.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			b.writeError(w, r, &statusError{status: http.StatusMethodNotAllowed,
				code: api.CodeMethodNotAllowed, msg: r.Method + " is not allowed here"})
			return
		}
		b.writeError(w, r, &statusError{status: http.StatusNotFound, code: api.CodeNotFound, msg: "no such route"})
	})
}

// probeWriter records the status and headers of the mux's fallback handler
// and discards its body.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

func (p *probeWriter) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}
