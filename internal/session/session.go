// Package session is the browser session of a control host, the admin host
// or the apex. A person signs in on the sign-in host and arrives with a
// single-use code; the host redeems it for a cookie only it receives, as an
// artifact host does. Here that cookie means a person at one of Kazari's
// own pages, never an artifact's script, so the host may also end the whole
// sign-in, and every form carries a token bound to the session. A form that
// makes a secret shown once also carries a single-use value, so that the
// resubmission a reload offers makes nothing new.
//
// What the person may do on the host is the host's to decide, on every
// request: this package says who they are and how they signed in, and
// leaves every page it refuses to the host to write.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/cookie"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/seal"
	"github.com/PeterBooker/kazari/internal/signin"
)

// ErrSignIn is a sign-in that did not complete: its code is unknown, used,
// expired or was started in another browser.
var ErrSignIn = errors.New("session: sign-in not completed")

// Provider checks and creates the sessions. signin.Service implements it;
// its methods return signin.ErrNotFound for a code or token that names
// nothing live.
type Provider interface {
	Redeem(ctx context.Context, code, host, nonce string) (signin.View, error)
	ControlSession(ctx context.Context, host, token string) (signin.ControlSession, error)
	SignOut(ctx context.Context, host, token string) error
	// LinkCode is a single-use code that signs handle in at the sign-in
	// host, for a person the host itself has just identified.
	LinkCode(ctx context.Context, handle string) (string, error)
}

// Options configures a Manager.
type Options struct {
	// Host is the control host's label: signin.HostAdmin or
	// signin.HostAccount.
	Host string
	// Cookie names the session cookie before the __Host- prefix, such as
	// kazari_admin; the nonce and flash cookies derive theirs from it.
	Cookie string
	// SignInURL is where a browser without a session is sent, e.g.
	// https://mcp.art.example.com/oauth/signin.
	SignInURL string
	// InsecureCookies drops the Secure attribute and the __Host- prefix,
	// for plain http on this machine.
	InsecureCookies bool
	// Now is the clock (default time.Now); cookie lifetimes count from it.
	Now func() time.Time
	// Telemetry counts the session authentications.
	Telemetry instrument.Providers
}

// Manager keeps the session cookie of one control host.
type Manager struct {
	p       Provider
	opts    Options
	auth    auth.Sessions
	once    *seal.Once
	cookies cookie.Host
	signIn  signin.Client
}

// New returns a Manager.
func New(p Provider, opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	cookies := cookie.Host{Insecure: opts.InsecureCookies}
	return &Manager{p: p, opts: opts, auth: auth.NewSessions(opts.Telemetry), once: seal.NewOnce(onceTTL, opts.Now),
		cookies: cookies, signIn: signin.Client{SignInURL: opts.SignInURL, Cookies: cookies, NonceCookie: opts.Cookie + "_nonce"}}
}

// Session is a person signed in on the host.
type Session struct {
	signin.ControlSession
	token string
	host  string
	m     *Manager
}

const (
	flashTTL = time.Minute
	// maxFlash bounds a flash code; it is a word the page maps to a
	// sentence, never text of its own.
	maxFlash = 48
)

// Get returns the session of the request's cookie, or nil when the browser
// has none that is live: the sign-in ended or expired, the person was
// disabled, or the API key they signed in with is no longer active.
func (m *Manager) Get(r *http.Request) (*Session, error) {
	token := m.cookies.Get(r, m.opts.Cookie)
	if token == "" {
		return nil, nil
	}
	cs, err := m.p.ControlSession(r.Context(), m.opts.Host, token)
	if errors.Is(err, signin.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Session{ControlSession: cs, token: token, host: m.opts.Host, m: m}, nil
}

// Presented reports whether the request carries a session cookie at all,
// live or not: what tells a session that has ended from a browser that
// never had one.
func (m *Manager) Presented(r *http.Request) bool {
	return m.cookies.Get(r, m.opts.Cookie) != ""
}

// SignIn sends the browser to the sign-in host, to come back to returnTo, a
// path on this host. It sets the nonce cookie the code will be bound to,
// reusing the browser's current one so that several tabs can sign in at
// once.
func (m *Manager) SignIn(w http.ResponseWriter, r *http.Request, returnTo string) {
	m.signIn.Start(w, r, m.opts.Host, signin.ReturnPath(returnTo), nil, http.StatusFound)
}

// Open ends a sign-in: it exchanges the code in the request's query for a
// session, sets the cookie and returns the session with the path the
// person wanted, which the caller redirects to. ErrSignIn when the code
// does not open a session in this browser. The nonce cookie stays until it
// expires: another tab's sign-in may still be on its way back with it.
func (m *Manager) Open(w http.ResponseWriter, r *http.Request) (*Session, string, error) {
	ctx := r.Context()
	v, err := m.p.Redeem(ctx, r.URL.Query().Get("code"), m.opts.Host, m.signIn.Nonce(r))
	if errors.Is(err, signin.ErrNotFound) {
		return nil, "", ErrSignIn
	}
	if err != nil {
		return nil, "", err
	}
	cs, err := m.p.ControlSession(ctx, m.opts.Host, v.Token)
	if errors.Is(err, signin.ErrNotFound) {
		return nil, "", ErrSignIn
	}
	if err != nil {
		return nil, "", err
	}
	m.cookies.Set(w, m.opts.Cookie, v.Token, v.ExpiresAt.Sub(m.opts.Now()))
	return &Session{ControlSession: cs, token: v.Token, host: m.opts.Host, m: m}, signin.ReturnPath(v.Path), nil
}

// Handoff signs the browser in as handle, a person this host has just
// identified itself, such as by a link they redeemed: it sends the browser
// through the sign-in host, which opens the browser's one sign-in session
// there and returns to returnTo with a code, as SignIn does. Every other
// host then joins that session, and signing out anywhere ends it. Should
// the code open nothing, the sign-in page asks for handle's password.
func (m *Manager) Handoff(ctx context.Context, w http.ResponseWriter, r *http.Request, handle, returnTo string) error {
	code, err := m.p.LinkCode(ctx, handle)
	if err != nil {
		return err
	}
	m.signIn.Start(w, r, m.opts.Host, signin.ReturnPath(returnTo), url.Values{signin.ParamHandoff: {code}, "login": {handle}}, http.StatusSeeOther)
	return nil
}

// End signs the browser out: the sign-in behind its cookie ends on every
// host, and the cookie is dropped.
func (m *Manager) End(w http.ResponseWriter, r *http.Request) error {
	if token := m.cookies.Get(r, m.opts.Cookie); token != "" {
		if err := m.p.SignOut(r.Context(), m.opts.Host, token); err != nil {
			return err
		}
	}
	m.cookies.Delete(w, m.opts.Cookie)
	return nil
}

// FormToken is the value every form of the session carries. It is derived
// from the session cookie, which script cannot read and no other host can
// set, so a page that cannot read this host's pages cannot make one; and it
// is not the cookie itself, so a page that leaks its markup leaks no
// session.
func (s *Session) FormToken() string {
	mac := hmac.New(sha256.New, []byte(s.token))
	mac.Write([]byte("kazari form token " + s.host))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ValidForm reports whether token is the session's form token.
func (s *Session) ValidForm(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.FormToken())) == 1
}

// SetFlash leaves a one-word note for the next page the browser loads, such
// as "disabled" after a redirect; the page turns it into its sentence.
// Anything that is not a short word of a-z, 0-9, - and _ is dropped: a flash
// never carries a secret or text an attacker chose.
func (m *Manager) SetFlash(w http.ResponseWriter, code string) {
	if !flashCode(code) {
		return
	}
	m.cookies.Set(w, m.opts.Cookie+"_flash", code, flashTTL)
}

// Flash returns the note SetFlash left, and clears it.
func (m *Manager) Flash(w http.ResponseWriter, r *http.Request) string {
	ck, err := r.Cookie(m.cookies.Name(m.opts.Cookie + "_flash"))
	if err != nil {
		return ""
	}
	m.cookies.Delete(w, m.opts.Cookie+"_flash")
	if !flashCode(ck.Value) {
		return ""
	}
	return ck.Value
}

func flashCode(s string) bool {
	if s == "" || len(s) > maxFlash {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
