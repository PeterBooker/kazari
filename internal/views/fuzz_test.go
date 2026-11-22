package views_test

import (
	"net/http"
	"testing"

	"github.com/PeterBooker/kazari/internal/views"
)

// Classify reads headers a visitor sends. It never panics, gives the same
// answer twice, and never counts a request without a user agent.
func FuzzClassify(f *testing.F) {
	f.Add("GET", "document", "text/html", "", firefox)
	f.Add("GET", "", "text/html", "prefetch", "Googlebot/2.1")
	f.Add("HEAD", "iframe", "*/*", "", "")
	f.Fuzz(func(t *testing.T, method, dest, accept, purpose, ua string) {
		r := &http.Request{Method: method, Header: http.Header{}}
		r.Header.Set("Sec-Fetch-Dest", dest)
		r.Header.Set("Accept", accept)
		r.Header.Set("Sec-Purpose", purpose)
		r.Header.Set("User-Agent", ua)
		got := views.Classify(r)
		if again := views.Classify(r); again != got {
			t.Fatalf("Classify gave %q, then %q", got, again)
		}
		if ua == "" && got == views.Counted {
			t.Fatalf("Classify counted a request without a user agent")
		}
	})
}
