package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PeterBooker/kazari/internal/middleware"
)

const pagesOrigin = "https://admin.art.example.com"

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name, origin, site string
		want               bool
	}{
		{name: "own origin", origin: pagesOrigin, want: true},
		{name: "own origin, other case", origin: "HTTPS://Admin.Art.Example.com", want: true},
		{name: "a sibling artifact", origin: "https://demo.art.example.com", site: "same-site"},
		{name: "another scheme", origin: "http://admin.art.example.com"},
		{name: "another port", origin: pagesOrigin + ":8443"},
		{name: "another site", origin: "https://evil.example"},
		{name: "null origin from the own page", origin: "null", site: "same-origin", want: true},
		{name: "null origin from a sibling", origin: "null", site: "same-site"},
		{name: "null origin from a sandboxed frame", origin: "null", site: "cross-site"},
		{name: "no origin, same-origin fetch", site: "same-origin", want: true},
		{name: "no origin, sibling fetch", site: "same-site"},
		{name: "a client that sends neither", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, pagesOrigin+"/people", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.site != "" {
				r.Header.Set("Sec-Fetch-Site", tt.site)
			}
			if got := middleware.SameOrigin(r, pagesOrigin); got != tt.want {
				t.Errorf("SameOrigin = %v, want %v", got, tt.want)
			}
		})
	}
}

// The origin is the configured one: a request that names another host in
// Host and Origin alike is not from this server's pages.
func TestSameOriginIgnoresHostHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://evil.example/people", nil)
	r.Header.Set("Origin", "https://evil.example")
	if middleware.SameOrigin(r, pagesOrigin) {
		t.Error("SameOrigin trusted the request's own Host")
	}
	if !middleware.SameHost(r) {
		t.Error("SameHost refused a post from the request's own host")
	}
	r.Header.Set("Origin", "https://other.example")
	if middleware.SameHost(r) {
		t.Error("SameHost accepted a post from another host")
	}
}

func TestPages(t *testing.T) {
	served := 0
	h := middleware.Pages(pagesOrigin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusNoContent)
	}))
	type hdr map[string]string
	tests := []struct {
		name, method string
		headers      hdr
		want         int
	}{
		{"typed address", "GET", hdr{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 204},
		{"own page's link", "GET", hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 204},
		{"own page's script", "GET", hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, 204},
		{"link from a chat", "GET", hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 204},
		{"redirect from an artifact host", "GET", hdr{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 204},
		{"a client without fetch metadata", "GET", nil, 204},
		{"an artifact's fetch", "GET", hdr{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "empty"}, 403},
		{"an artifact's image tag", "GET", hdr{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}, 403},
		{"an artifact's frame", "GET", hdr{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, 403},
		{"another site's script", "GET", hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, 403},
		{"own form", "POST", hdr{"Origin": pagesOrigin, "Sec-Fetch-Site": "same-origin"}, 204},
		{"own form under no-referrer", "POST", hdr{"Origin": "null", "Sec-Fetch-Site": "same-origin"}, 204},
		{"an artifact's form", "POST", hdr{"Origin": "https://demo.art.example.com", "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"an artifact's fetch post", "POST", hdr{"Origin": "https://demo.art.example.com", "Sec-Fetch-Site": "same-site"}, 403},
		{"another site's form", "POST", hdr{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 403},
		{"a sandboxed frame's form", "POST", hdr{"Origin": "null", "Sec-Fetch-Site": "cross-site"}, 403},
		{"delete from an artifact", "DELETE", hdr{"Origin": "https://demo.art.example.com"}, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := served
			r := httptest.NewRequest(tt.method, pagesOrigin+"/people", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
			if (served > before) != (tt.want == 204) {
				t.Errorf("handler ran = %v", served > before)
			}
			for name, want := range map[string]string{
				"Content-Security-Policy":      middleware.PageCSP,
				"X-Frame-Options":              "DENY",
				"Cross-Origin-Resource-Policy": "same-origin",
			} {
				if got := w.Header().Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestPageCSP(t *testing.T) {
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	if middleware.PageCSP != want {
		t.Errorf("PageCSP = %q", middleware.PageCSP)
	}
}
