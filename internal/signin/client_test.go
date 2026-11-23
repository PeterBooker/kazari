package signin_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/cookie"
	"github.com/PeterBooker/kazari/internal/signin"
)

var client = signin.Client{SignInURL: "https://mcp.art.test/oauth/signin", Cookies: cookie.Host{}, NonceCookie: "kazari_nonce"}

// start runs Client.Start for r and returns the nonce cookie it set and the
// sign-in page's query.
func start(t *testing.T, r *http.Request, extra url.Values) (*http.Cookie, url.Values) {
	t.Helper()
	rec := httptest.NewRecorder()
	client.Start(rec, r, "demo", "/report?x=1", extra, http.StatusFound)
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("Start: status %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != client.SignInURL {
		t.Fatalf("Start: Location %q, %v; want the sign-in page", resp.Header.Get("Location"), err)
	}
	cks := resp.Cookies()
	if len(cks) != 1 || cks[0].Name != "__Host-kazari_nonce" || cks[0].MaxAge != 600 {
		t.Fatalf("Start set %+v, want the nonce cookie for 10 minutes", cks)
	}
	return cks[0], loc.Query()
}

func s256(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func TestClientStart(t *testing.T) {
	ck, q := start(t, httptest.NewRequest(http.MethodGet, "https://demo.art.test/report?x=1", nil), url.Values{"login": {"ana"}})
	if len(ck.Value) != 43 {
		t.Fatalf("nonce %q, want 32 random bytes in base64url", ck.Value)
	}
	want := url.Values{"host": {"demo"}, "path": {"/report?x=1"}, "nonce": {s256(ck.Value)}, "login": {"ana"}}
	if diff := cmp.Diff(want, q); diff != "" {
		t.Errorf("sign-in query (-want +got):\n%s", diff)
	}
}

// A browser's nonce is reused, so that several tabs can sign in at once;
// one that is not a nonce is replaced.
func TestClientStartReusesTheNonce(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://demo.art.test/", nil)
	nonce := "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm8"
	r.AddCookie(&http.Cookie{Name: "__Host-kazari_nonce", Value: nonce})
	if got := client.Nonce(r); got != nonce {
		t.Errorf("Nonce = %q, want %q", got, nonce)
	}
	ck, q := start(t, r, nil)
	if ck.Value != nonce || q.Get("nonce") != s256(nonce) {
		t.Errorf("Start with a nonce cookie set %q and sent %q, want %q and its S256", ck.Value, q.Get("nonce"), nonce)
	}

	r = httptest.NewRequest(http.MethodGet, "https://demo.art.test/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-kazari_nonce", Value: "short"})
	if ck, _ := start(t, r, nil); ck.Value == "short" || len(ck.Value) != 43 {
		t.Errorf("Start kept the malformed nonce: %q", ck.Value)
	}
}
