package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID gives every request an id: the incoming X-Request-ID when it is
// 8 to 128 safe characters (letters, digits, '-', '_', '.', ':'), else 16
// random bytes in hex. The id is stored in the context and echoed in the
// response header.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFrom returns the id stored by RequestID, or "" if there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func validRequestID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error and always fills b.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
