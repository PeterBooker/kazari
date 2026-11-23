package signin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"maps"
	"net/http"
	"net/url"
	"time"

	"github.com/PeterBooker/kazari/internal/cookie"
)

// nonceTTL is how long a browser keeps a host's nonce cookie: as long as a
// sign-in may take.
const nonceTTL = 10 * time.Minute

// Client is a host's side of a sign-in: it sends a browser to the sign-in
// page with the S256 hash of a nonce cookie, and reads that cookie back
// when the browser returns with a code for Redeem.
type Client struct {
	// SignInURL is the sign-in page, e.g.
	// https://mcp.art.example.com/oauth/signin.
	SignInURL string
	// Cookies are the host's, and NonceCookie the name of its nonce cookie
	// before the __Host- prefix.
	Cookies     cookie.Host
	NonceCookie string
}

// Start sends the browser to sign in for host, to come back to path there,
// with extra added to the sign-in page's query. It sets the nonce cookie,
// reusing the browser's current one so that several tabs can sign in at
// once.
func (c Client) Start(w http.ResponseWriter, r *http.Request, host, path string, extra url.Values, status int) {
	nonce := c.Nonce(r)
	if len(nonce) != nonceLen {
		var b [32]byte
		_, _ = rand.Read(b[:]) // crypto/rand panics rather than fail
		nonce = base64.RawURLEncoding.EncodeToString(b[:])
	}
	c.Cookies.Set(w, c.NonceCookie, nonce, nonceTTL)
	sum := sha256.Sum256([]byte(nonce))
	q := url.Values{
		"host":  {host},
		"path":  {path},
		"nonce": {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	maps.Copy(q, extra)
	http.Redirect(w, r, c.SignInURL+"?"+q.Encode(), status)
}

// Nonce returns the value of the browser's nonce cookie, "" without one.
func (c Client) Nonce(r *http.Request) string {
	return c.Cookies.Get(r, c.NonceCookie)
}
