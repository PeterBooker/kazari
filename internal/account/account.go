// Package account serves the pages a person opens for themselves, on the
// apex under /account/: the three link pages that set a password (the
// first-run setup, an invite, a reset), the page that sets one with a
// recovery code, and the account page with its password, passkeys,
// recovery codes, API keys and connected apps.
//
// A link page needs no session: the link is the credential. Opening one
// consumes nothing; the token moves from the address into a short-lived
// cookie, and the form's POST uses it up. The account page needs the
// session of the apex, which the sign-in host opens as it does for any
// other host, and every form of it carries the session's token. A new key
// and a new set of recovery codes are the things rendered in the answer to a
// POST, once.
package account

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/session"
	"github.com/PeterBooker/kazari/internal/theme"
	"github.com/PeterBooker/kazari/internal/user"
)

// Prefix is the path every page of this package lives under.
const Prefix = "/account/"

// CallbackPath is where a sign-in for the apex ends; the sign-in host's
// callback for oauth.HostAccount is this path on the apex.
const CallbackPath = Prefix + "callback"

const (
	assetPrefix   = Prefix + "assets/"
	sessionCookie = "kazari_account"
	linkCookie    = "kazari_link"
	// linkCookieTTL is how long a person has to choose a password once the
	// link is open; the link itself lives on if they take longer.
	linkCookieTTL = 15 * time.Minute
	maxFormBytes  = 64 << 10
	// formTokenField is the hidden field every form of these pages carries.
	formTokenField = session.FormField
)

// How wrong link tokens and wrong current passwords slow the next attempt:
// five cost nothing, then the wait doubles from a second to a minute.
const (
	freeFailures = 5
	baseDelay    = time.Second
	maxDelay     = time.Minute
)

// Grants lists and ends what a person's sign-ins left behind: the apps they
// connected and their browser sessions. oauth.Service implements it.
type Grants interface {
	// ListGrants returns the person's connected apps, each as its newest
	// live refresh token; FamilyID names the grant.
	ListGrants(ctx context.Context, handle string) ([]oauth.Token, error)
	RevokeGrant(ctx context.Context, id int64) error
	GetClient(ctx context.Context, id string) (*oauth.Client, error)
	// EndSessions ends the person's browser sessions except keep.
	EndSessions(ctx context.Context, handle string, keep int64) error
}

// Pages lists a person's artifacts and changes them as the person may.
// artifact.Service implements it.
type Pages interface {
	List(ctx context.Context, actor artifact.Actor, q artifact.ListQuery) (artifact.ListPage, error)
	Get(ctx context.Context, actor artifact.Actor, slug string) (*artifact.Artifact, []artifact.Version, error)
	SetVisibility(ctx context.Context, actor artifact.Actor, slug, v string) (*artifact.Artifact, error)
	Publish(ctx context.Context, actor artifact.Actor, slug string, version int) (*artifact.Artifact, error)
	MarkStale(ctx context.Context, actor artifact.Actor, slug string, grace time.Duration) (*artifact.Artifact, error)
	UnmarkStale(ctx context.Context, actor artifact.Actor, slug string) (*artifact.Artifact, error)
	Delete(ctx context.Context, actor artifact.Actor, slug string, hard bool) error
	Update(ctx context.Context, actor artifact.Actor, req artifact.UpdateRequest) (*artifact.Artifact, *artifact.Version, error)
}

// Sharing creates, lists and revokes a person's share links.
// artifact.ShareLinks implements it.
type Sharing interface {
	CreateShareLink(ctx context.Context, actor artifact.Actor, slug, label string, ttl time.Duration, password string) (*artifact.ShareLink, string, error)
	ListShareLinks(ctx context.Context, actor artifact.Actor, slug string) ([]artifact.ShareLink, error)
	RevokeShareLink(ctx context.Context, actor artifact.Actor, slug, prefix string) error
}

// Passkeys adds passkeys to accounts. passkey.Service implements it.
type Passkeys interface {
	Registration(ctx context.Context, u *user.User) (passkey.Ceremony, error)
	Register(ctx context.Context, u *user.User, token, response, name string) (*user.Passkey, error)
	Confirmation(ctx context.Context, u *user.User) (passkey.Ceremony, bool, error)
	Confirm(ctx context.Context, u *user.User, token, response string) error
}

// Deps are the collaborators of the account pages. Audit, Artifacts and
// Passkeys may be nil; the account page then leaves their sections out.
// ShareLinks is set whenever Artifacts is.
type Deps struct {
	Sessions   session.Provider
	Users      *user.Service
	Grants     Grants
	Artifacts  Pages
	ShareLinks Sharing
	Audit      audit.Reader
	Passkeys   Passkeys
	Links      present.Links
	Logger     *slog.Logger
}

// Options configures the account pages.
type Options struct {
	// URL is the apex origin, such as https://art.example.com. The origin
	// check uses it, never a request's Host.
	URL string
	// SignInURL is the sign-in page, e.g.
	// https://mcp.art.example.com/oauth/signin.
	SignInURL string
	// AdminURL is where the first admin goes once setup is done; "" when the
	// dashboard is off.
	AdminURL string
	// MCPURL and APIURL appear in the snippets shown with a new key.
	MCPURL, APIURL string
	// LocalAccounts turns the link pages and the password section on.
	LocalAccounts bool
	// MaxShareLinkTTL is the longest a share link may live; 0 is the
	// artifact service's default.
	MaxShareLinkTTL time.Duration
	// MaxKeyTTL, when set, is the longest a key may live.
	MaxKeyTTL time.Duration
	// InsecureCookies drops the Secure attribute and the __Host- prefix,
	// for development over plain http.
	InsecureCookies bool
	Now             func() time.Time
	Telemetry       instrument.Providers
}

type server struct {
	d      Deps
	o      Options
	now    func() time.Time
	log    *slog.Logger
	sess   *session.Manager
	tmpl   *template.Template
	assets theme.Assets
	host   string // of Options.URL, shown at the foot of a page
	// linkCSP is the policy of a link's form, whose submission goes on to
	// the sign-in host or the dashboard; recoverCSP that of the recovery
	// form, whose submission goes on to the sign-in host.
	linkCSP, recoverCSP string

	// linkDelay counts wrong link tokens per client address; the other two
	// count wrong current passwords per account and per address.
	linkDelay, passwordDelay, addressDelay *ratelimit.Backoff
}

//go:embed templates/*.html
var templateFS embed.FS

// New returns the handler of every path under Prefix, for the apex.
func New(d Deps, o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	backoff := func(scope string) *ratelimit.Backoff {
		return ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: freeFailures, Base: baseDelay, Max: maxDelay,
			Now: o.Now, Scope: scope, Telemetry: o.Telemetry})
	}
	s := &server{
		d: d, o: o, now: o.Now, log: log,
		sess: session.New(d.Sessions, session.Options{
			Host: oauth.HostAccount, Cookie: sessionCookie, SignInURL: o.SignInURL, InsecureCookies: o.InsecureCookies, Now: o.Now,
			Telemetry: o.Telemetry,
		}),
		tmpl:      template.Must(theme.Templates().Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		assets:    theme.URLs(assetPrefix),
		linkDelay: backoff(ratelimit.ScopeIP), passwordDelay: backoff(ratelimit.ScopeAccount), addressDelay: backoff(ratelimit.ScopeIP),
	}
	if u, err := url.Parse(o.URL); err == nil {
		s.host = u.Host
	}
	s.linkCSP = formAction(middleware.PageCSP, o.SignInURL, o.AdminURL)
	s.recoverCSP = formAction(middleware.PageCSP, o.SignInURL)

	mux := http.NewServeMux()
	mux.Handle("GET "+assetPrefix, theme.Handler(assetPrefix))
	pages := middleware.Pages(o.URL)
	handle := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, noStore(pages(fn)))
	}
	if o.LocalAccounts {
		for _, p := range []user.Purpose{user.PurposeInvite, user.PurposeReset, user.PurposeSetup} {
			handle("GET "+Prefix+string(p), s.linkPage(p))
			handle("POST "+Prefix+string(p), s.redeemLink(p))
		}
		handle("POST "+Prefix+"password", s.signedIn(s.changePassword))
		handle("POST "+Prefix+"recovery-codes", s.signedIn(s.once(s.newRecoveryCodes)))
		if d.Passkeys != nil {
			handle("POST "+Prefix+"passkeys", s.signedIn(s.addPasskey))
			handle("POST "+Prefix+"passkeys/{id}/remove", s.signedIn(s.removePasskey))
		}
		handle("GET "+Prefix+"recover", s.recoverPage)
		handle("POST "+Prefix+"recover", s.recover)
	}
	handle("POST "+Prefix+"sessions/end", s.signedIn(s.endSessions))
	handle("GET "+CallbackPath, s.callback)
	handle("GET /account", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix, http.StatusMovedPermanently)
	})
	handle("GET "+Prefix+"{$}", s.signedIn(s.accountPage))
	handle("POST "+Prefix+"signout", s.signOut)
	handle("POST "+Prefix+"keys", s.signedIn(s.once(s.issueKey)))
	if d.Artifacts != nil {
		s.pageRoutes(handle)
	}
	handle("GET "+Prefix+"keys/{prefix}/rotate", s.signedIn(s.rotatePage))
	handle("POST "+Prefix+"keys/{prefix}/rotate", s.signedIn(s.once(s.rotateKey)))
	handle("GET "+Prefix+"keys/{prefix}/revoke", s.signedIn(s.revokeKeyPage))
	handle("POST "+Prefix+"keys/{prefix}/revoke", s.signedIn(s.revokeKey))
	handle("GET "+Prefix+"apps/{id}/revoke", s.signedIn(s.revokeAppPage))
	handle("POST "+Prefix+"apps/{id}/revoke", s.signedIn(s.revokeApp))
	handle(Prefix, func(w http.ResponseWriter, r *http.Request) { s.message(w, r, http.StatusNotFound, "not-found") })
	return middleware.Routed(mux)
}

// noStore keeps every page out of caches: they show a person's keys and
// sessions, and some answer a link's token.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// visit is one request of a signed-in person.
type visit struct {
	sess *session.Session
	user *user.User
}

// signedIn runs fn for the person whose session the request carries, with
// them as the principal of every call it makes. Without a session a page
// load goes to sign in and anything else is refused; a form must carry the
// session's token.
func (s *server) signedIn(fn func(http.ResponseWriter, *http.Request, visit)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sess, err := s.sess.Require(w, r)
		var u *user.User
		if err == nil {
			u, err = s.d.Users.GetUser(ctx, sess.Handle)
			if errors.Is(err, user.ErrNotFound) {
				err = s.sess.SignedOut(w, r)
			}
		}
		if err == nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			err = sess.CheckForm(w, r)
		}
		switch {
		case errors.Is(err, session.ErrSignInStarted):
			return
		case errors.Is(err, session.ErrSignedOut):
			s.message(w, r, http.StatusUnauthorized, "unauthorized")
			return
		case errors.Is(err, session.ErrBadForm):
			s.message(w, r, http.StatusBadRequest, "bad-form")
			return
		case errors.Is(err, session.ErrFormToken):
			s.message(w, r, http.StatusForbidden, "forbidden")
			return
		case err != nil:
			s.fail(w, r, "load session", err)
			return
		}
		// Member scopes at most, whatever the role: nothing on this page
		// acts as an admin. A sign-in with a key acts with that key's.
		scopes := auth.MemberScopes
		if sess.KeyPrefix != "" {
			st, err := s.d.Users.KeyStatus(ctx, sess.KeyPrefix)
			if err != nil && !errors.Is(err, user.ErrKeyNotFound) {
				s.fail(w, r, "load key", err)
				return
			}
			scopes = slices.DeleteFunc(slices.Clone(auth.MemberScopes), func(sc string) bool { return !slices.Contains(st.Scopes, sc) })
		}
		p := auth.Principal{Handle: u.Handle, Scopes: scopes, Source: auth.SourceSession, KeyID: sess.KeyPrefix, QuotaBytes: u.QuotaBytes}
		s.sess.Admit(ctx, p)
		fn(w, r.WithContext(auth.ContextWithPrincipal(ctx, p)), visit{sess: sess, user: u})
	}
}

// once runs fn, which makes a key shown once, only for a form sent for the
// first time: a reload of the page that showed the key sends it again.
func (s *server) once(fn func(http.ResponseWriter, *http.Request, visit)) func(http.ResponseWriter, *http.Request, visit) {
	return func(w http.ResponseWriter, r *http.Request, v visit) {
		switch err := v.sess.Claim(r.PostForm.Get(session.OnceField)); {
		case errors.Is(err, session.ErrFormUsed):
			s.message(w, r, http.StatusConflict, "form-used")
		case err != nil:
			s.message(w, r, http.StatusForbidden, "form-expired")
		default:
			fn(w, r, v)
		}
	}
}

// base is what every template reads.
type base struct {
	Assets    theme.Assets
	Host      string
	FormToken string
	Once      string // the single-use value of a form that makes a key
	Person    string // the signed-in person's name
	Handle    string
	Role      string
}

// Door is the head of a front-door page. The title is an argument so that
// it is written in the template, with every other string.
func (b base) Door(title string) theme.Page {
	return theme.Page{Title: title, Assets: b.Assets}
}

// Shell is the frame of a signed-in page; label names its navigation and
// nav its one destination.
func (b base) Shell(title, label, nav string) theme.Shell {
	return theme.Shell{
		Title:   title,
		Assets:  b.Assets,
		Home:    Prefix,
		Host:    b.Host,
		Label:   label,
		Nav:     []theme.Link{{Label: nav, Href: Prefix, Current: true}},
		Person:  b.Person,
		Detail:  b.Handle + " · " + b.Role,
		SignOut: theme.Form{Action: Prefix + "signout", Hidden: map[string]string{formTokenField: b.FormToken}},
	}
}

func (s *server) base() base {
	return base{Assets: s.assets, Host: s.host}
}

func (s *server) baseFor(v visit) base {
	b := s.base()
	b.FormToken = v.sess.FormToken()
	b.Once = v.sess.Once()
	b.Handle, b.Role = v.user.Handle, string(v.user.Role)
	b.Person = v.user.DisplayName
	if b.Person == "" {
		b.Person = v.user.Handle
	}
	return b
}

func (s *server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	if code := present.PageCode(status); code != "" {
		middleware.SetErrorCode(r.Context(), code)
	}
	if err := theme.Render(w, r, s.tmpl, name, status, data); err != nil {
		s.log.ErrorContext(r.Context(), "account: render page", "page", name, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// messagePage is a front-door page that says one thing; Kind picks its
// words in the template.
type messagePage struct {
	base
	Kind    string
	Seconds int // of a wait
	// Text is the sentence of a refusal by the artifact service, and Back
	// the page to go back to.
	Text, Back string
}

func (s *server) message(w http.ResponseWriter, r *http.Request, status int, kind string) {
	s.render(w, r, status, "message", messagePage{base: s.base(), Kind: kind})
}

// wait answers a request that came too soon after failures.
func (s *server) wait(w http.ResponseWriter, r *http.Request, d time.Duration) {
	secs := max(1, int((d+time.Second-1)/time.Second))
	w.Header().Set("Retry-After", ratelimit.RetryAfter(d))
	s.render(w, r, http.StatusTooManyRequests, "message", messagePage{base: s.base(), Kind: "wait", Seconds: secs})
}

// busy answers a password that was not checked or set because the server
// is hashing as many as it does at once: nothing changed, and the same
// form works a moment later.
func (s *server) busy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	s.message(w, r, http.StatusServiceUnavailable, "busy")
	middleware.SetErrorCode(r.Context(), api.CodeBusy)
}

// fail logs an error that is not the person's doing and answers with a
// page that says nothing about it.
func (s *server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), "account: "+what, "err", err)
	s.message(w, r, http.StatusInternalServerError, "error")
}

// done redirects to the account page, which shows flash as its one-line
// confirmation.
func (s *server) done(w http.ResponseWriter, r *http.Request, flash string) {
	s.sess.SetFlash(w, flash)
	http.Redirect(w, r, Prefix, http.StatusSeeOther)
}

// callback ends a sign-in for the apex.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	_, err := s.sess.Callback(w, r)
	if errors.Is(err, session.ErrSignIn) {
		s.message(w, r, http.StatusBadRequest, "signin-failed")
		return
	}
	if err != nil {
		s.fail(w, r, "redeem sign-in", err)
	}
}

// signOut ends the sign-in on every host. A browser whose session already
// ended is signed out all the same.
func (s *server) signOut(w http.ResponseWriter, r *http.Request) {
	switch err := s.sess.SignOut(w, r); {
	case errors.Is(err, session.ErrBadForm), errors.Is(err, session.ErrFormToken):
		s.message(w, r, http.StatusForbidden, "forbidden")
	case err != nil:
		s.fail(w, r, "sign out", err)
	default:
		s.message(w, r, http.StatusOK, "signed-out")
	}
}

// safeOrigin matches an origin that can be put in a CSP source list as is.
var safeOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$|^http://\[::1\](:[0-9]+)?$`)

// formAction returns policy with the origins of targets added to its
// form-action, for a form whose submission is redirected to them.
func formAction(policy string, targets ...string) string {
	const self = "form-action 'self'"
	list := self
	for _, t := range targets {
		u, err := url.Parse(t)
		if err != nil || t == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host
		if safeOrigin.MatchString(origin) && !strings.Contains(list, " "+origin) {
			list += " " + origin
		}
	}
	return strings.Replace(policy, self, list, 1)
}

var funcs = template.FuncMap{
	"scopeWords": func(sc string) string {
		if w := scopeWords[sc]; w != "" {
			return w
		}
		return sc
	},
	"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"stamp":   func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") },
}
