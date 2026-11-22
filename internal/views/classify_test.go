package views_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PeterBooker/kazari/internal/views"
)

const firefox = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"

// pageRequest is a browser opening a page in a tab.
func pageRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://demo.art.test/", nil)
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("User-Agent", firefox)
	return r
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		edit func(r *http.Request)
		want views.Reason
	}{
		{"page in a tab", func(*http.Request) {}, views.Counted},
		{"no fetch metadata", func(r *http.Request) { r.Header.Del("Sec-Fetch-Dest") }, views.Counted},
		{"head", func(r *http.Request) { r.Method = http.MethodHead }, views.NotPage},
		{"range", func(r *http.Request) { r.Header.Set("Range", "bytes=0-10") }, views.NotPage},
		{"frame", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "iframe") }, views.NotPage},
		{"fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "empty") }, views.NotPage},
		{"no metadata, no html", func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Dest")
			r.Header.Set("Accept", "*/*")
		}, views.NotPage},
		{"chrome prefetch", func(r *http.Request) { r.Header.Set("Sec-Purpose", "prefetch") }, views.Prefetch},
		{"chrome prerender", func(r *http.Request) { r.Header.Set("Sec-Purpose", "prefetch;prerender") }, views.Prefetch},
		{"firefox prefetch", func(r *http.Request) { r.Header.Set("X-Moz", "prefetch") }, views.Prefetch},
		{"purpose prefetch", func(r *http.Request) { r.Header.Set("Purpose", "Prefetch") }, views.Prefetch},
		{"no user agent", func(r *http.Request) { r.Header.Del("User-Agent") }, views.Bot},
		{"googlebot", func(r *http.Request) {
			r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
		}, views.Bot},
		{"slack preview", func(r *http.Request) {
			r.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)")
		}, views.Bot},
		{"teams preview", func(r *http.Request) {
			r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.1; WOW64) SkypeUriPreview Preview/0.5")
		}, views.Bot},
		{"headless chrome", func(r *http.Request) {
			r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 HeadlessChrome/140.0.0.0 Safari/537.36")
		}, views.Bot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := pageRequest()
			tt.edit(r)
			if got := views.Classify(r); got != tt.want {
				t.Errorf("Classify(%s %v) = %q, want %q", r.Method, r.Header, got, tt.want)
			}
		})
	}
}
