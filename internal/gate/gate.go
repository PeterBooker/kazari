// Package gate guards gated artifacts (visibility members) on their own
// hosts. It decides whether a request may see the page, sends a browser
// without access to sign in at the MCP host, and answers the paths every
// artifact host reserves under /.kazari/: the end of a sign-in, share links
// and leaving this host. It cannot end a sign-in: member-authored script
// runs on these hosts, so that happens only on the MCP host.
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
	"github.com/PeterBooker/kazari/internal/instrument"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/theme"
	"github.com/PeterBooker/kazari/internal/user"
	"github.com/PeterBooker/kazari/internal/views"
)

// Reserved paths on every artifact host.
const (
	Prefix       = "/" + artifact.ReservedDir + "/"
	CallbackPath = Prefix + "callback" // ?code=: the end of a sign-in
	SharePath    = Prefix + "share"    // ?token=: opens a share link; a POST gives its password
	SignOutPath  = Prefix + "signout"
)

// Cookie names before the __Host- prefix: the visitor's access, the nonce
// that binds a sign-in to the browser that started it, and the token of a
// share link whose password form is open, kept off the address bar.
const (
	viewCookie  = "kazari_view"
	nonceCookie = "kazari_nonce"
	nonceTTL    = 10 * time.Minute // as long as a sign-in may take
	nonceLen    = 43               // 32 random bytes, base64url
	shareCookie = "kazari_share"
	shareTTL    = 10 * time.Minute // as long as typing a password may take
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
	EndView(ctx context.Context, host, token string) error
}

// Links checks share links. artifact.Service implements it; every method
// returns artifact.ErrLinkNotFound for a token or cookie that does not open
// slug, and UnlockShareLink artifact.ErrForbidden for a wrong password.
type Links interface {
	CheckShareLink(ctx context.Context, slug, cookie string) (*artifact.ShareLink, error)
	// OpenShareLink returns the view cookie, or "" for a link that asks for
	// a password first.
	OpenShareLink(ctx context.Context, slug, token string) (*artifact.ShareLink, string, error)
	UnlockShareLink(ctx context.Context, slug, token, password string) (*artifact.ShareLink, string, error)
}

// Options configures a Gate.
type Options struct {
	// SignInURL is where a browser without access is sent, e.g.
	// https://mcp.art.example.com/oauth/signin.
	SignInURL string
	// SignOutURL is the page that ends a whole sign-in, e.g.
	// https://mcp.art.example.com/oauth/signout.
	SignOutURL string
	// InsecureCookies drops the Secure attribute and the __Host- prefix,
	// for development over plain http.
	InsecureCookies bool
	// Now is the clock (default time.Now); cookie lifetimes count from it.
	Now func() time.Time
	// Logger receives errors; nil uses slog.Default().
	Logger    *slog.Logger
	Telemetry instrument.Providers
}

// Gate implements site.Gate.
type Gate struct {
	sessions Sessions
	links    Links
	opts     Options
	log      *slog.Logger
	requests metric.Int64Counter
	// results are the counter's attribute sets as the option slices Add
	// takes, built once: every page view passes the gate, and a slice built
	// per call would be allocated each time.
	results map[string][]metric.AddOption
	// guessDelay slows down guesses at a link's password from one address,
	// and linkDelay, which lets many more go first, those from every address
	// together: one visitor's wrong guesses then hold up the link's other
	// visitors only once they are many.
	guessDelay, linkDelay *ratelimit.Backoff
}

// The waits between guesses at a share link's password: a few cost nothing,
// then each failure doubles the wait, up to linkMaxDelay.
const (
	guessFreeFailures = 3
	linkFreeFailures  = 30
	linkBaseDelay     = time.Second
	linkMaxDelay      = time.Minute
)

// Results of the gate, the values of kazari.gate.result.
const (
	resultPublic    = string(views.ViaPublic)
	resultMember    = string(views.ViaMember)
	resultShareLink = string(views.ViaShareLink)
	resultRedirect  = "redirect"
	resultDenied    = "denied"
)

const resultKey = attribute.Key("kazari.gate.result")

const scope = "github.com/PeterBooker/kazari/internal/gate"

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(theme.Templates().New("page").Parse(pageHTML))

// New returns a Gate.
func New(sessions Sessions, links Links, opts Options) *Gate {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	results := map[string][]metric.AddOption{}
	for _, r := range []string{resultPublic, resultMember, resultShareLink, resultRedirect, resultDenied} {
		results[r] = []metric.AddOption{metric.WithAttributes(resultKey.String(r))}
	}
	backoff := func(free int, scope string) *ratelimit.Backoff {
		return ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: free, Base: linkBaseDelay, Max: linkMaxDelay,
			Now: opts.Now, Scope: scope, Telemetry: opts.Telemetry})
	}
	return &Gate{sessions: sessions, links: links, opts: opts, log: log, results: results,
		requests: instrument.Handle(opts.Telemetry.Meter(scope).Int64Counter("kazari.gate.requests",
			metric.WithUnit("{request}"), metric.WithDescription("Requests for artifact pages by what the gate decided."))),
		guessDelay: backoff(guessFreeFailures, ratelimit.ScopeIP), linkDelay: backoff(linkFreeFailures, ratelimit.ScopeAccount)}
}

// Allow reports whether r may be served p on host, the artifact host's
// label (<slug> or <slug>--v<N>), and who was let in. A public or unlisted
// artifact is always allowed, unless the version is staged. Otherwise the
// visitor needs a view cookie that opens this host, and for a staged version
// one from a sign-in;
// without one, Allow answers the request and returns false: a page
// navigation is sent to sign in, anything else (a script, an image, a
// fetch, a frame, a client such as curl) gets 401.
func (g *Gate) Allow(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string) (views.Access, bool) {
	if !p.Gated() {
		g.decided(r.Context(), resultPublic)
		return views.Access{Via: views.ViaPublic}, true
	}
	a, err := g.admitted(r, p, host)
	if err != nil {
		g.log.ErrorContext(r.Context(), "gate: check access", "host", host, "err", err)
		g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
		return views.Access{}, false
	}
	if a.Via != "" {
		g.decided(r.Context(), string(a.Via))
		return a, true
	}
	if navigation(r) {
		g.decided(r.Context(), resultRedirect)
		g.toSignIn(w, r, host)
		return views.Access{}, false
	}
	g.decided(r.Context(), resultDenied)
	hdr := w.Header()
	setNoStore(hdr)
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("X-Robots-Tag", "noindex")
	body := "This page is only open to members of this Kazari instance. Open it in a browser to sign in, " +
		"or ask its owner for a share link.\n"
	if p.Staged {
		body = "This version is not published yet, and only members of this Kazari instance can open it. " +
			"Open it in a browser to sign in.\n"
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusUnauthorized)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
	return views.Access{}, false
}

// Admits reports whether r's view cookie opens host. Unlike Allow it answers
// nothing and counts nothing.
func (g *Gate) Admits(r *http.Request, p *artifact.Published, host string) (bool, error) {
	a, err := g.admitted(r, p, host)
	return a.Via != "", err
}

// decided counts the gate's decision and puts it on the request's span.
func (g *Gate) decided(ctx context.Context, result string) {
	g.requests.Add(ctx, 1, g.results[result]...)
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(resultKey.String(result))
	}
}

// admitted returns how the request's view cookie opens host, by a member's
// sign-in or a share link, or a zero Access when it does not.
func (g *Gate) admitted(r *http.Request, p *artifact.Published, host string) (views.Access, error) {
	ck, err := r.Cookie(g.name(viewCookie))
	if err != nil || ck.Value == "" {
		return views.Access{}, nil //nolint:nilerr // the only error is http.ErrNoCookie, and no cookie opens nothing
	}
	a, notFound := views.Access{Via: views.ViaMember}, oauth.ErrNotFound
	if artifact.IsLinkCookie(ck.Value) {
		// A share link is for the people the page is published to, and a
		// staged version is not published yet.
		if p.Staged {
			return views.Access{}, nil
		}
		a.Via, notFound = views.ViaShareLink, artifact.ErrLinkNotFound
		_, err = g.links.CheckShareLink(r.Context(), p.Slug, ck.Value)
	} else {
		a.Handle, err = g.sessions.Viewer(r.Context(), host, ck.Value)
	}
	switch {
	case errors.Is(err, notFound):
		return views.Access{}, nil
	case err != nil:
		return views.Access{}, err
	}
	return a, nil
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
	var nonce string
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
		if g.method(w, r, http.MethodGet, http.MethodPost) {
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
	if slices.Contains(allowed, r.Method) {
		return true
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

// share opens a share link: the view cookie it gives lasts until the link
// expires, and the browser goes on to the page without the token in its
// address bar or history. A link with a password moves its token to a
// short-lived cookie instead and comes back here without it, to a form that
// posts the password.
func (g *Gate) share(w http.ResponseWriter, r *http.Request, p *artifact.Published) {
	if p.Staged {
		g.page(w, r, http.StatusForbidden, page{Title: "Not published yet",
			Message: "A share link opens the published page, and this version of it is not published yet.",
			Link:    "/", LinkText: "Sign in to open it"})
		return
	}
	if r.Method == http.MethodPost {
		g.unlock(w, r, p)
		return
	}
	token, fromAddress := r.URL.Query().Get("token"), true
	if token == "" {
		token, fromAddress = g.cookie(r, shareCookie), false
	}
	l, cookie, err := g.links.OpenShareLink(r.Context(), p.Slug, token)
	switch {
	case err != nil:
		g.setCookie(w, shareCookie, "", -1)
		g.linkFailed(w, r, p, err)
	case cookie != "":
		g.opened(w, l, cookie)
	case fromAddress:
		g.setCookie(w, shareCookie, token, min(shareTTL, l.ExpiresAt.Sub(g.opts.Now())))
		setNoStore(w.Header())
		w.Header().Set("Location", SharePath)
		w.WriteHeader(http.StatusSeeOther)
	default:
		g.page(w, r, http.StatusOK, passwordPage(""))
	}
}

// unlock checks the password posted for the link whose token the share
// cookie holds, as slowly as the guesses before it make it wait.
func (g *Gate) unlock(w http.ResponseWriter, r *http.Request, p *artifact.Published) {
	if !middleware.SameHost(r) {
		g.page(w, r, http.StatusForbidden, page{Title: "Request rejected", Message: "This form was not sent from this page."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUnlockBytes)
	if err := r.ParseForm(); err != nil {
		g.page(w, r, http.StatusBadRequest, page{Title: "Request rejected", Message: "The form could not be read."})
		return
	}
	token, password := g.cookie(r, shareCookie), r.PostForm.Get("password")
	if !artifact.IsLinkToken(token) {
		g.page(w, r, http.StatusForbidden, page{Title: "Form expired",
			Message: "This password form has expired. Open the link you were sent again."})
		return
	}
	if password == "" {
		g.page(w, r, http.StatusBadRequest, passwordPage("Enter the password."))
		return
	}
	link := p.Slug + " " + strings.SplitN(token, "_", 3)[1]
	guess := link + " " + ratelimit.IPKey(r)
	attempt, wait := ratelimit.BeginPair(r.Context(), g.guessDelay, guess, g.linkDelay, link)
	if wait > 0 {
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		g.page(w, r, http.StatusTooManyRequests, passwordPage("Too many tries. Try again in "+ratelimit.WaitText(wait)+"."))
		return
	}
	l, cookie, err := g.links.UnlockShareLink(r.Context(), p.Slug, token, password)
	switch {
	case errors.Is(err, artifact.ErrForbidden):
		attempt.Failed()
		g.page(w, r, http.StatusForbidden, passwordPage("That password is not right."))
	case errors.Is(err, user.ErrBusy):
		attempt.Done()
		w.Header().Set("Retry-After", "5")
		g.page(w, r, http.StatusServiceUnavailable, passwordPage("The server is busy. Try again in a moment."))
	case errors.Is(err, context.Canceled):
		attempt.Done() // the visitor left; there is no one to answer
	case err != nil:
		attempt.Done()
		g.setCookie(w, shareCookie, "", -1)
		g.linkFailed(w, r, p, err)
	default:
		attempt.Done()
		g.guessDelay.Reset(guess)
		g.linkDelay.Reset(link)
		g.setCookie(w, shareCookie, "", -1)
		g.opened(w, l, cookie)
	}
}

// cookie returns the value of the host's cookie named base, "" for none.
func (g *Gate) cookie(r *http.Request, base string) string {
	if ck, err := r.Cookie(g.name(base)); err == nil {
		return ck.Value
	}
	return ""
}

// maxUnlockBytes bounds the password form: a password of at most
// artifact.ShareLinkPasswordMax four-byte characters, encoded.
const maxUnlockBytes = 16 << 10

// opened sets the view cookie of an opened link and goes on to the page.
func (g *Gate) opened(w http.ResponseWriter, l *artifact.ShareLink, cookie string) {
	g.setCookie(w, viewCookie, cookie, l.ExpiresAt.Sub(g.opts.Now()))
	setNoStore(w.Header())
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusFound)
}

func (g *Gate) linkFailed(w http.ResponseWriter, r *http.Request, p *artifact.Published, err error) {
	if errors.Is(err, artifact.ErrLinkNotFound) {
		g.page(w, r, http.StatusForbidden, page{Title: "Link not valid",
			Message: "This share link has expired or was revoked. Ask the person who sent it for a new one."})
		return
	}
	g.log.ErrorContext(r.Context(), "gate: open share link", "slug", p.Slug, "err", err)
	g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
}

// passwordPage asks for the password of the link whose token the share
// cookie holds.
func passwordPage(problem string) page {
	return page{Title: "Enter the password",
		Message: "This page needs the password that came with its link.",
		Unlock:  &unlockForm{Action: SharePath, Error: problem}}
}

// signOut shows a button (GET) or leaves this host (POST): its view session
// ends and its cookie is dropped, a share link's too. That is all an
// artifact host can do. Script on this host passes the same-origin check
// and could read a token from the form, so nothing here can tell a person
// from the page, and a sign-in ends only on the MCP host's page, which
// every answer links to. A sign-in that goes on opens this host again with
// no prompt, so the pages say so.
func (g *Gate) signOut(w http.ResponseWriter, r *http.Request, host string) {
	const everywhere = "Sign out of this Kazari instance"
	if r.Method == http.MethodGet {
		if ck, err := r.Cookie(g.name(viewCookie)); err != nil || ck.Value == "" {
			g.page(w, r, http.StatusOK, page{Title: "Not signed in here",
				Message: "This browser has not opened this page signed in.",
				Link:    g.opts.SignOutURL, LinkText: everywhere})
			return
		}
		g.page(w, r, http.StatusOK, page{Title: "Sign out",
			Message: "This ends this browser's access to this page only. Any sign-in to this Kazari instance stays, and opens the page again with no prompt.",
			Form:    SignOutPath, Link: g.opts.SignOutURL, LinkText: everywhere})
		return
	}
	if !middleware.SameHost(r) {
		g.page(w, r, http.StatusForbidden, page{Title: "Request rejected", Message: "This form was not sent from this page."})
		return
	}
	if ck, err := r.Cookie(g.name(viewCookie)); err == nil && !artifact.IsLinkCookie(ck.Value) {
		if err := g.sessions.EndView(r.Context(), host, ck.Value); err != nil {
			g.log.ErrorContext(r.Context(), "gate: end view", "host", host, "err", err)
			g.page(w, r, http.StatusInternalServerError, page{Title: "Something went wrong", Message: "Please try again later."})
			return
		}
	}
	g.setCookie(w, viewCookie, "", -1)
	g.page(w, r, http.StatusOK, page{Title: "Signed out of this page",
		Message: "This browser's access to this page has ended. Any sign-in to this Kazari instance is still active.",
		Link:    g.opts.SignOutURL, LinkText: everywhere})
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
	Link, LinkText string      // an optional link under the message
	Form           string      // the action of a sign-out button, if any
	Unlock         *unlockForm // the password form of a share link, if any
}

// unlockForm asks for a share link's password.
type unlockForm struct {
	Action string
	Error  string // why the last try failed, if it did
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
