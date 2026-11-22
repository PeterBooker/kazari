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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/oauth"
)

// ErrSignIn is a sign-in that did not complete: its code is unknown, used,
// expired or was started in another browser.
var ErrSignIn = errors.New("session: sign-in not completed")

// Provider checks and creates the sessions. oauth.Service implements it;
// its methods return oauth.ErrNotFound for a code or token that names
// nothing live.
type Provider interface {
	Redeem(ctx context.Context, code, host, nonce string) (oauth.View, error)
	ControlSession(ctx context.Context, host, token string) (oauth.ControlSession, error)
	SignOut(ctx context.Context, host, token string) error
	// StartControl opens a session on the control host for a person the
	// host itself has just identified.
	StartControl(ctx context.Context, host, handle string) (oauth.View, error)
}

// Options configures a Manager.
type Options struct {
	// Host is the control host's label: oauth.HostAdmin or
	// oauth.HostAccount.
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
	p    Provider
	opts Options
	auth auth.Sessions
	once *once
}

// New returns a Manager.
func New(p Provider, opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{p: p, opts: opts, auth: auth.NewSessions(opts.Telemetry), once: newOnce()}
}

// Session is a person signed in on the host.
type Session struct {
	oauth.ControlSession
	token string
	host  string
	m     *Manager
}

const (
	nonceTTL = 10 * time.Minute // as long as a sign-in may take
	nonceLen = 43               // 32 random bytes, base64url
	flashTTL = time.Minute
	// maxFlash bounds a flash code; it is a word the page maps to a
	// sentence, never text of its own.
	maxFlash = 48
)

// Get returns the session of the request's cookie, or nil when the browser
// has none that is live: the sign-in ended or expired, the person was
// disabled, or the API key they signed in with is no longer active.
func (m *Manager) Get(r *http.Request) (*Session, error) {
	ck, err := r.Cookie(m.name(""))
	if err != nil || ck.Value == "" {
		return nil, nil //nolint:nilerr // the only error is http.ErrNoCookie, and no cookie is no session
	}
	cs, err := m.p.ControlSession(r.Context(), m.opts.Host, ck.Value)
	if errors.Is(err, oauth.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Session{ControlSession: cs, token: ck.Value, host: m.opts.Host, m: m}, nil
}

// Presented reports whether the request carries a session cookie at all,
// live or not: what tells a session that has ended from a browser that
// never had one.
func (m *Manager) Presented(r *http.Request) bool {
	ck, err := r.Cookie(m.name(""))
	return err == nil && ck.Value != ""
}

// SignIn sends the browser to the sign-in host, to come back to returnTo, a
// path on this host. It sets the nonce cookie the code will be bound to,
// reusing the browser's current one so that several tabs can sign in at
// once.
func (m *Manager) SignIn(w http.ResponseWriter, r *http.Request, returnTo string) {
	var nonce string
	if ck, err := r.Cookie(m.name("_nonce")); err == nil && len(ck.Value) == nonceLen {
		nonce = ck.Value
	} else {
		var b [32]byte
		_, _ = rand.Read(b[:]) // crypto/rand panics rather than fail
		nonce = base64.RawURLEncoding.EncodeToString(b[:])
	}
	m.setCookie(w, "_nonce", nonce, nonceTTL)
	sum := sha256.Sum256([]byte(nonce))
	q := url.Values{
		"host":  {m.opts.Host},
		"path":  {oauth.ReturnPath(returnTo)},
		"nonce": {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	http.Redirect(w, r, m.opts.SignInURL+"?"+q.Encode(), http.StatusFound)
}

// Open ends a sign-in: it exchanges the code in the request's query for a
// session, sets the cookie and returns the session with the path the
// person wanted, which the caller redirects to. ErrSignIn when the code
// does not open a session in this browser. The nonce cookie stays until it
// expires: another tab's sign-in may still be on its way back with it.
func (m *Manager) Open(w http.ResponseWriter, r *http.Request) (*Session, string, error) {
	nonce := ""
	if ck, err := r.Cookie(m.name("_nonce")); err == nil {
		nonce = ck.Value
	}
	ctx := r.Context()
	v, err := m.p.Redeem(ctx, r.URL.Query().Get("code"), m.opts.Host, nonce)
	if errors.Is(err, oauth.ErrNotFound) {
		return nil, "", ErrSignIn
	}
	if err != nil {
		return nil, "", err
	}
	cs, err := m.p.ControlSession(ctx, m.opts.Host, v.Token)
	if errors.Is(err, oauth.ErrNotFound) {
		return nil, "", ErrSignIn
	}
	if err != nil {
		return nil, "", err
	}
	m.setCookie(w, "", v.Token, v.ExpiresAt.Sub(m.opts.Now()))
	return &Session{ControlSession: cs, token: v.Token, host: m.opts.Host, m: m}, oauth.ReturnPath(v.Path), nil
}

// Start signs the browser in as handle on this host alone, without the
// sign-in host: for a person the host has just identified itself, such as
// by a link they redeemed.
func (m *Manager) Start(ctx context.Context, w http.ResponseWriter, handle string) error {
	v, err := m.p.StartControl(ctx, m.opts.Host, handle)
	if err != nil {
		return err
	}
	m.setCookie(w, "", v.Token, v.ExpiresAt.Sub(m.opts.Now()))
	return nil
}

// End signs the browser out: the sign-in behind its cookie ends on every
// host, and the cookie is dropped.
func (m *Manager) End(w http.ResponseWriter, r *http.Request) error {
	if ck, err := r.Cookie(m.name("")); err == nil && ck.Value != "" {
		if err := m.p.SignOut(r.Context(), m.opts.Host, ck.Value); err != nil {
			return err
		}
	}
	m.setCookie(w, "", "", -1)
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
	m.setCookie(w, "_flash", code, flashTTL)
}

// Flash returns the note SetFlash left, and clears it.
func (m *Manager) Flash(w http.ResponseWriter, r *http.Request) string {
	ck, err := r.Cookie(m.name("_flash"))
	if err != nil {
		return ""
	}
	m.setCookie(w, "_flash", "", -1)
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

// Navigation reports whether r loads a page into a browser tab, where a
// redirect to sign in helps. Browsers that send no Fetch Metadata are
// judged by their Accept header.
func Navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// name returns a cookie's name: the base with suffix, under the __Host-
// prefix unless cookies are insecure. Browsers accept a __Host- cookie only
// when it is Secure, on path / and without Domain, so script in an artifact
// on a sibling host cannot plant one.
func (m *Manager) name(suffix string) string {
	if m.opts.InsecureCookies {
		return m.opts.Cookie + suffix
	}
	return "__Host-" + m.opts.Cookie + suffix
}

// setCookie sets a host-only cookie; ttl <= 0 deletes it.
func (m *Manager) setCookie(w http.ResponseWriter, suffix, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.name(suffix),
		Value:    value,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !m.opts.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}
