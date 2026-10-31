// Package httpapi serves the user API on the api host and the admin API on the
// admin host. Domain errors become statuses in one place, present.Error.
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

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/user"
)

// jsonLimit caps JSON bodies that cannot carry inline files; those use
// UserOptions.MaxBodyBytes.
const jsonLimit = 1 << 20

// ErrGrantNotFound is what a GrantStore returns (or wraps) for an unknown
// grant id, so DELETE /v1/grants/{id} answers 404.
var ErrGrantNotFound = errors.New("httpapi: grant not found")

// statusError is for failures that are HTTP concerns, not domain errors.
type statusError struct {
	status     int
	code       string
	msg        string
	retryAfter time.Duration // sent as Retry-After on 429
}

func (e *statusError) Error() string { return e.code + ": " + e.msg }

// invalid is a 422 whose message reaches the client.
func invalid(format string, args ...any) error {
	return artifact.Errorf(artifact.ErrInvalidArgument, format, args...)
}

// apiFunc returns its failure for the wrapper to write.
type apiFunc func(w http.ResponseWriter, r *http.Request, p auth.Principal) error

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

func (b *base) serve(fn apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		if err := fn(w, r, p); err != nil {
			b.writeError(w, r, err)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the single place an error becomes a response. A 5xx is logged
// with its cause; its body only ever carries a generic message.
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

// decodeJSON rejects unknown fields, trailing data and bodies over limit
// (413). An empty body is a 422 unless optional, which leaves v as it is.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any, optional bool) error {
	body := http.MaxBytesReader(w, r.Body, limit)
	return decodeStrict(body, v, optional)
}

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

// jsonFallback replaces the mux's plain-text 404 and 405 pages with JSON,
// keeping its Allow header.
func jsonFallback(mux *http.ServeMux, b *base) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		middleware.SetRoute(r.Context(), pattern)
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
