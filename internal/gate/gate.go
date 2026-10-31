// Package gate guards gated artifacts (visibility members) on their own
// hosts. It decides whether a request may see the page, sends a browser
// without access to sign in at the MCP host, and answers the paths every
// artifact host reserves under /.kazari/: the end of a sign-in, share links
// and sign-out.
//
// A visitor's access is one host-only cookie, __Host-kazari_view, set by
// this package and by nothing else: either a view session token from a
// sign-in (oauth.Service checks it, for this host only) or the token of a
// share link for this artifact. It is HttpOnly, so the artifact's own
// script cannot read it, and the __Host- prefix stops a sibling artifact
// from planting one with a Domain attribute.
package gate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
)

// Reserved paths on every artifact host.
const (
	Prefix       = "/" + artifact.ReservedDir + "/"
	CallbackPath = Prefix + "callback" // ?code=: the end of a sign-in
	SharePath    = Prefix + "share"    // ?token=: opens a share link
	SignOutPath  = Prefix + "signout"
)

// Cookie names before the __Host- prefix: the visitor's access, and the
// nonce that binds a sign-in to the browser that started it.
const (
	viewCookie  = "kazari_view"
	nonceCookie = "kazari_nonce"
	nonceTTL    = 10 * time.Minute // as long as a sign-in may take
	nonceLen    = 43               // 32 random bytes, base64url
)

// pageCSP is the policy of the pages this package serves on an artifact
// host; they are the instance's, not the artifact's.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'"

// Sessions checks and creates view sessions for signed-in people.
// oauth.Service implements it; every method returns oauth.ErrNotFound for
// a code or token that does not grant access.
type Sessions interface {
	Redeem(ctx context.Context, code, host, nonce string) (oauth.View, error)
	Viewer(ctx context.Context, host, token string) (string, error)
	SignOut(ctx context.Context, host, token string) error
}

// Links checks share links. artifact.Service implements it; both methods
// return artifact.ErrLinkNotFound for a token that does not open slug.
type Links interface {
	CheckShareLink(ctx context.Context, slug, token string) (*artifact.ShareLink, error)
	OpenShareLink(ctx context.Context, slug, token string) (*artifact.ShareLink, error)
}

// Options configures a Gate.
type Options struct {
	// SignInURL is where a browser without access is sent, e.g.
	// https://mcp.art.example.com/oauth/signin.
	SignInURL string
	// InsecureCookies drops the Secure attribute and the __Host- prefix,
	// for development over plain http.
	InsecureCookies bool
	// Now is the clock (default time.Now); cookie lifetimes count from it.
	Now func() time.Time
	// Logger receives errors; nil uses slog.Default().
	Logger *slog.Logger
}

// Gate implements site.Gate.
type Gate struct {
	sessions Sessions
	links    Links
	opts     Options
	log      *slog.Logger
}

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// New returns a Gate.
func New(sessions Sessions, links Links, opts Options) *Gate {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Gate{sessions: sessions, links: links, opts: opts, log: log}
}

// Allow reports whether r may be served p on host, the artifact host's
// label (<slug> or <slug>--v<N>). A public or unlisted artifact is always
// allowed. Otherwise the visitor needs a view cookie that opens this host;
// without one, Allow answers the request and returns false: a page
// navigation is sent to sign in, anything else (a script, an image, a
// fetch, a frame, a client such as curl) gets 401.
func (g *Gate) Allow(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string) bool {
	if !p.Visibility.Gated() {
		return true
	}
	ok, err := g.admitted(r, p, host)
	if err != nil {
		g.log.ErrorContext(r.Context(), "gate: check access", "host", host, "err", err)
		g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
		return false
	}
	if ok {
		return true
	}
	if navigation(r) {
		g.toSignIn(w, r, host)
		return false
	}
	hdr := w.Header()
	setNoStore(hdr)
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("X-Robots-Tag", "noindex")
	body := "This page is only open to members of this Kazari instance. Open it in a browser to sign in, " +
		"or ask its owner for a share link.\n"
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusUnauthorized)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
	return false
}

// admitted reports whether the request's view cookie opens host.
func (g *Gate) admitted(r *http.Request, p *artifact.Published, host string) (bool, error) {
	ck, err := r.Cookie(g.name(viewCookie))
	if err != nil || ck.Value == "" {
		return false, nil
	}
	if artifact.IsLinkToken(ck.Value) {
		_, err := g.links.CheckShareLink(r.Context(), p.Slug, ck.Value)
		if errors.Is(err, artifact.ErrLinkNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	_, err = g.sessions.Viewer(r.Context(), host, ck.Value)
	if errors.Is(err, oauth.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// navigation reports whether r loads a page into a browser tab, where a
// redirect to sign in helps. Browsers that send no Fetch Metadata are
// judged by their Accept header.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// toSignIn sets the nonce cookie, reusing the browser's current one so
// that several tabs can sign in at once, and redirects to the sign-in host.
func (g *Gate) toSignIn(w http.ResponseWriter, r *http.Request, host string) {
	nonce := ""
	if ck, err := r.Cookie(g.name(nonceCookie)); err == nil && len(ck.Value) == nonceLen {
		nonce = ck.Value
	} else {
		var b [32]byte
		_, _ = rand.Read(b[:]) // never fails (crypto/rand panics on failure)
		nonce = base64.RawURLEncoding.EncodeToString(b[:])
	}
	g.setCookie(w, nonceCookie, nonce, nonceTTL)
	sum := sha256.Sum256([]byte(nonce))
	q := url.Values{
		"host":  {host},
		"path":  {r.URL.RequestURI()},
		"nonce": {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	setNoStore(w.Header())
	http.Redirect(w, r, g.opts.SignInURL+"?"+q.Encode(), http.StatusFound)
}

// ServeReserved answers a request under Prefix on host, for artifact p.
func (g *Gate) ServeReserved(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string) {
	switch r.URL.Path {
	case CallbackPath, SharePath, SignOutPath:
		middleware.SetRoute(r.Context(), r.URL.Path)
	}
	switch r.URL.Path {
	case CallbackPath:
		if g.method(w, r, http.MethodGet) {
			g.callback(w, r, host)
		}
	case SharePath:
		if g.method(w, r, http.MethodGet) {
			g.share(w, r, p)
		}
	case SignOutPath:
		if g.method(w, r, http.MethodGet, http.MethodPost) {
			g.signOut(w, r, host)
		}
	default:
		g.page(w, r, http.StatusNotFound, page{Title: "Not found", Message: "There is nothing at this address."})
	}
}

// method answers 405 unless r uses one of allowed. HEAD is not implied:
// these paths change state, and a link scanner's HEAD must not.
func (g *Gate) method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	g.page(w, r, http.StatusMethodNotAllowed, page{Title: "Method not allowed", Message: "This address only opens in a browser."})
	return false
}

// callback redeems the code a sign-in sent back, sets the view cookie and
// returns to the page the visitor wanted. The nonce cookie stays until it
// expires: another tab's sign-in may still be on its way back with it.
func (g *Gate) callback(w http.ResponseWriter, r *http.Request, host string) {
	nonce := ""
	if ck, err := r.Cookie(g.name(nonceCookie)); err == nil {
		nonce = ck.Value
	}
	v, err := g.sessions.Redeem(r.Context(), r.URL.Query().Get("code"), host, nonce)
	if errors.Is(err, oauth.ErrNotFound) {
		g.page(w, r, http.StatusBadRequest, page{Title: "Sign-in failed",
			Message: "This sign-in has expired, was already used or was started in another browser.",
			Link:    "/", LinkText: "Open the page again"})
		return
	}
	if err != nil {
		g.log.ErrorContext(r.Context(), "gate: redeem", "host", host, "err", err)
		g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
		return
	}
	g.setCookie(w, viewCookie, v.Token, v.ExpiresAt.Sub(g.opts.Now()))
	// Location is set as is: http.Redirect would clean the path, and
	// cleaning can turn a path on this host into one a browser reads as
	// another host's.
	setNoStore(w.Header())
	w.Header().Set("Location", oauth.ReturnPath(v.Path))
	w.WriteHeader(http.StatusFound)
}

// share opens a share link: its token becomes the view cookie until the
// link expires, and the browser goes on to the page without the token in
// its address bar or history.
func (g *Gate) share(w http.ResponseWriter, r *http.Request, p *artifact.Published) {
	token := r.URL.Query().Get("token")
	l, err := g.links.OpenShareLink(r.Context(), p.Slug, token)
	if errors.Is(err, artifact.ErrLinkNotFound) {
		g.page(w, r, http.StatusForbidden, page{Title: "Link not valid",
			Message: "This share link has expired or was revoked. Ask the person who sent it for a new one."})
		return
	}
	if err != nil {
		g.log.ErrorContext(r.Context(), "gate: open share link", "slug", p.Slug, "err", err)
		g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
		return
	}
	g.setCookie(w, viewCookie, token, l.ExpiresAt.Sub(g.opts.Now()))
	setNoStore(w.Header())
	http.Redirect(w, r, "/", http.StatusFound)
}

// signOut shows a sign-out button (GET) or signs out (POST): a sign-in
// ends in this browser on every host, and a share link's cookie on this
// host is dropped. The POST must come from this host's own page. Only a
// host the browser opened holds a cookie to sign out with, so elsewhere
// the page says so instead of offering a button that would do nothing.
func (g *Gate) signOut(w http.ResponseWriter, r *http.Request, host string) {
	if r.Method == http.MethodGet {
		if ck, err := r.Cookie(g.name(viewCookie)); err != nil || ck.Value == "" {
			g.page(w, r, http.StatusOK, page{Title: "Not signed in here",
				Message: "This browser has not opened this page signed in. Sign out from a members page you have open."})
			return
		}
		g.page(w, r, http.StatusOK, page{Title: "Sign out",
			Message: "Sign out of this Kazari instance in this browser?", Form: SignOutPath})
		return
	}
	if !sameOrigin(r) {
		g.page(w, r, http.StatusForbidden, page{Title: "Request rejected", Message: "This form was not sent from this page."})
		return
	}
	if ck, err := r.Cookie(g.name(viewCookie)); err == nil && !artifact.IsLinkToken(ck.Value) {
		if err := g.sessions.SignOut(r.Context(), host, ck.Value); err != nil {
			g.log.ErrorContext(r.Context(), "gate: sign out", "host", host, "err", err)
			g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
			return
		}
	}
	g.setCookie(w, viewCookie, "", -1)
	g.page(w, r, http.StatusOK, page{Title: "Signed out", Message: "You are signed out of this Kazari instance in this browser."})
}

// sameOrigin reports whether a form post comes from a page on the request's
// own host: Origin names it, or, without Origin, Sec-Fetch-Site says
// same-origin (or is absent, from a client that sends neither).
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func (g *Gate) name(base string) string {
	if g.opts.InsecureCookies {
		return base
	}
	return "__Host-" + base
}

// setCookie sets a host-only cookie; ttl <= 0 deletes it.
func (g *Gate) setCookie(w http.ResponseWriter, base, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     g.name(base),
		Value:    value,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !g.opts.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// page is the data of the instance page shown on an artifact host.
type page struct {
	Title, Message string
	Link, LinkText string // an optional link under the message
	Form           string // the action of a sign-out button, if any
}

func (g *Gate) page(w http.ResponseWriter, r *http.Request, status int, p page) {
	var b strings.Builder
	if err := pageTmpl.Execute(&b, p); err != nil {
		g.log.ErrorContext(r.Context(), "gate: render page", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	setNoStore(hdr)
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", pageCSP)
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("X-Robots-Tag", "noindex")
	hdr.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(b.String()))
	}
}

// setNoStore marks a gate response as never cached and never sending a
// referrer: redirects and pages here carry codes and tokens in their URLs.
func setNoStore(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}
