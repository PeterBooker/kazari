package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+">")
				next.ServeHTTP(w, r)
				order = append(order, "<"+name)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("a"), mw("b"), mw("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "a> b> c> handler <c <b <a"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}

	rec := httptest.NewRecorder()
	Chain(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	generated := regexp.MustCompile(`^[0-9a-f]{32}$`)
	tests := []struct {
		name     string
		incoming string
		reuse    bool
	}{
		{"none", "", false},
		{"valid", "abc-123_DEF.456:7", true},
		{"min length", "12345678", true},
		{"max length", strings.Repeat("a", 128), true},
		{"too short", "1234567", false},
		{"too long", strings.Repeat("a", 129), false},
		{"space", "abc 12345678", false},
		{"newline", "abc12345\n678", false},
		{"slash", "abc/12345678", false},
		{"non-ascii", "abcdéf12345", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(RequestIDHeader)
			if got != seen {
				t.Errorf("header %q != context %q", got, seen)
			}
			if tt.reuse {
				if got != tt.incoming {
					t.Errorf("id = %q, want incoming %q", got, tt.incoming)
				}
			} else if !generated.MatchString(got) {
				t.Errorf("id = %q, want 32 hex chars", got)
			}
		})
	}

	if id := RequestIDFrom(context.Background()); id != "" {
		t.Errorf("RequestIDFrom(empty) = %q, want empty", id)
	}
	if a, b := newRequestID(), newRequestID(); a == b {
		t.Errorf("two generated ids are equal: %q", a)
	}
}

func TestRealIP(t *testing.T) {
	tests := []struct {
		name   string
		proxy  bool
		remote string
		xff    string
		xri    string
		want   string
	}{
		{"direct", false, "192.0.2.1:1234", "", "", "192.0.2.1"},
		{"direct ignores headers", false, "192.0.2.1:1234", "203.0.113.9", "203.0.113.8", "192.0.2.1"},
		{"direct ipv6", false, "[2001:db8::1]:443", "", "", "2001:db8::1"},
		{"direct without port", false, "192.0.2.1", "", "", "192.0.2.1"},
		{"proxy last xff", true, "10.0.0.1:1", "198.51.100.1, 10.0.0.2, 203.0.113.9", "203.0.113.8", "203.0.113.9"},
		{"proxy ignores client-supplied left entries", true, "10.0.0.1:1", "1.2.3.4, 203.0.113.9", "", "203.0.113.9"},
		{"proxy single xff with spaces", true, "10.0.0.1:1", "  203.0.113.9  ", "", "203.0.113.9"},
		{"proxy xff ipv6", true, "10.0.0.1:1", "2001:db8::2", "", "2001:db8::2"},
		{"proxy mapped ipv4", true, "10.0.0.1:1", "::ffff:203.0.113.9", "", "203.0.113.9"},
		{"proxy x-real-ip", true, "10.0.0.1:1", "", "203.0.113.8", "203.0.113.8"},
		{"proxy invalid xff falls back to x-real-ip", true, "10.0.0.1:1", "garbage", "203.0.113.8", "203.0.113.8"},
		{"proxy no headers", true, "10.0.0.1:1", "", "", "10.0.0.1"},
		{"proxy invalid headers", true, "10.0.0.1:1", "nope", "nope", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := RealIP(tt.proxy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = ClientIP(r)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				req.Header.Set("X-Real-IP", tt.xri)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.7:99"
	if got := ClientIP(req); got != "192.0.2.7" {
		t.Errorf("ClientIP without middleware = %q, want 192.0.2.7", got)
	}
}

func newJSONLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogger(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		status  float64
		bytes   float64
		level   string
	}{
		{"implicit 200", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hello")) }, 200, 5, "INFO"},
		{"nothing written", func(w http.ResponseWriter, r *http.Request) {}, 200, 0, "INFO"},
		{"explicit 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("no"))
		}, 404, 2, "INFO"},
		{"second WriteHeader ignored", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
		}, 201, 0, "INFO"},
		{"informational then final", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusAccepted)
		}, 202, 0, "INFO"},
		{"5xx at error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, 502, 0, "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, buf := newJSONLogger()
			h := Chain(tt.handler, RequestID(), RealIP(false), Logger(l))
			req := httptest.NewRequest(http.MethodPost, "http://api.art.test/v1/things?q=1", nil)
			req.RemoteAddr = "192.0.2.1:5555"
			req.Header.Set("User-Agent", "test-agent/1.0")
			req.Header.Set(RequestIDHeader, "req-12345678")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			lines := decodeLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want 1: %s", len(lines), buf)
			}
			got := lines[0]
			want := map[string]any{
				"level":      tt.level,
				"msg":        "request",
				"method":     "POST",
				"host":       "api.art.test",
				"path":       "/v1/things",
				"status":     tt.status,
				"bytes":      tt.bytes,
				"request_id": "req-12345678",
				"ip":         "192.0.2.1",
				"user_agent": "test-agent/1.0",
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			if d, ok := got["duration_ms"].(float64); !ok || d < 0 {
				t.Errorf("duration_ms = %v, want a non-negative number", got["duration_ms"])
			}
		})
	}
}

func TestLoggerTruncatesUserAgent(t *testing.T) {
	l, buf := newJSONLogger()
	h := Logger(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("User-Agent", strings.Repeat("a", 199)+"é"+strings.Repeat("b", 50))
	h.ServeHTTP(httptest.NewRecorder(), req)

	ua := decodeLines(t, buf)[0]["user_agent"].(string)
	if ua != strings.Repeat("a", 199) {
		t.Errorf("user_agent = %q (%d bytes), want 199 a's", ua, len(ua))
	}

	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"},
		{"aé", 3, "aé"},
	} {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (f *flushRecorder) SetWriteDeadline(t time.Time) error {
	f.deadline = t
	return nil
}

func TestResponseWriterInterfaces(t *testing.T) {
	l, _ := newJSONLogger()
	under := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	deadline := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped writer is not an http.Flusher")
		}
		_, _ = w.Write([]byte("x"))
		f.Flush()
		if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
			t.Errorf("ResponseController.SetWriteDeadline: %v", err)
		}
	}), Logger(l), Recover(l))
	h.ServeHTTP(under, httptest.NewRequest(http.MethodGet, "/", nil))

	if !under.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}
	if !under.deadline.Equal(deadline) {
		t.Errorf("deadline = %v, want %v", under.deadline, deadline)
	}
}

func TestRecover(t *testing.T) {
	t.Run("panic before writing", func(t *testing.T) {
		l, buf := newJSONLogger()
		h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("ETag", `"x"`)
			panic("boom")
		}), RequestID(), Logger(l), Recover(l))
		req := httptest.NewRequest(http.MethodGet, "/p", nil)
		req.Header.Set(RequestIDHeader, "req-panic-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if got := rec.Body.String(); got != internalErrorBody {
			t.Errorf("body = %q, want %q", got, internalErrorBody)
		}
		var body struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal" || body.Error.Message != "internal error" {
			t.Errorf("body does not decode to the internal error: %+v, %v", body, err)
		}
		for k, v := range map[string]string{
			"Content-Type":  "application/json",
			"Cache-Control": "no-store",
			"ETag":          "",
		} {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("header %s = %q, want %q", k, got, v)
			}
		}

		lines := decodeLines(t, buf)
		if len(lines) != 2 {
			t.Fatalf("got %d log lines, want panic + request: %s", len(lines), buf)
		}
		p := lines[0]
		if p["msg"] != "panic" || p["level"] != "ERROR" || p["panic"] != "boom" || p["request_id"] != "req-panic-1" {
			t.Errorf("panic line = %v", p)
		}
		if s, _ := p["stack"].(string); !strings.Contains(s, "TestRecover") {
			t.Errorf("stack does not name the panicking test: %q", s)
		}
		if lines[1]["status"] != float64(500) || lines[1]["level"] != "ERROR" {
			t.Errorf("request line = %v, want status 500 at ERROR", lines[1])
		}
	})

	t.Run("panic after writing", func(t *testing.T) {
		l, buf := newJSONLogger()
		h := Recover(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("partial"))
			panic(errors.New("late"))
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
			t.Errorf("got %d %q, want the response left as written", rec.Code, rec.Body)
		}
		if !strings.Contains(buf.String(), `"panic":"late"`) {
			t.Errorf("panic not logged: %s", buf)
		}
	})

	t.Run("no panic", func(t *testing.T) {
		l, buf := newJSONLogger()
		h := Recover(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" || buf.Len() != 0 {
			t.Errorf("got %d %q, log %q", rec.Code, rec.Body, buf)
		}
	})

	t.Run("abort handler re-panics", func(t *testing.T) {
		l, buf := newJSONLogger()
		h := Recover(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler", v)
			}
			if buf.Len() != 0 {
				t.Errorf("abort was logged: %s", buf)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		t.Error("ServeHTTP returned instead of panicking")
	})
}

func TestSecurityHeaders(t *testing.T) {
	defaults := map[string]string{
		"Cross-Origin-Opener-Policy": "same-origin",
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "no-referrer",
		"Cache-Control":              "no-store",
	}
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		override map[string]string
	}{
		{"defaults", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }, nil},
		{"defaults without body", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }, nil},
		{"handler overrides cache", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=60")
			_, _ = w.Write([]byte("x"))
		}, map[string]string{"Cache-Control": "public, max-age=60"}},
		{"handler overrides referrer", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Referrer-Policy", "same-origin")
		}, map[string]string{"Referrer-Policy": "same-origin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			SecurityHeaders()(tt.handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			for k, v := range defaults {
				if o, ok := tt.override[k]; ok {
					v = o
				}
				if got := rec.Header().Values(k); len(got) != 1 || got[0] != v {
					t.Errorf("header %s = %q, want [%q]", k, got, v)
				}
			}
		})
	}
}
