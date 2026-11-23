// Package signin is the browser sign-in host for every other host: a
// session cookie on the MCP host, and per host a single-use code that the
// host redeems in-process (Redeem) for a cookie only it receives. Those
// hosts are the artifact hosts with gated pages and the control hosts, the
// admin host and the apex (HostAdmin, HostAccount).
//
// A host sends a person to PathSignIn with its label, the path to return to
// and the S256 hash of a nonce cookie it set (Start). A browser with a live
// session cookie goes straight back with a single-use code; otherwise the
// person signs in first: at the identity provider, or without one with the
// password or a passkey of their built-in account, or an API key. The same
// form signs a person in for an MCP client's pending authorization, which
// then goes on to the authorization server's consent page (Requests).
//
// On an artifact host a sign-in ends at PathSignOut and nowhere else:
// member-authored script runs there, so an artifact host can end only its
// own view session (EndView). A control host runs Kazari's pages alone and
// ends the whole sign-in (SignOut).
package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/cookie"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	PathSignIn  = "/oauth/signin"
	PathSignOut = "/oauth/signout"
	// PathRecover sends a browser on to Options.RecoverURL.
	PathRecover = "/oauth/recover"
	// PathAssets is where the sign-in host serves the stylesheet, the
	// script and the fonts its pages load: the pages allow their own origin
	// and nothing else.
	PathAssets = "/oauth/assets/"

	// The labels of the control hosts. "@" is in no artifact label, so
	// neither can be mistaken for an artifact host.
	HostAdmin   = "@admin"   // the dashboard, at admin.public_url
	HostAccount = "@account" // the account pages, on the apex
	controlMark = "@"

	// ParamHandoff is the sign-in page's parameter that carries a LinkCode.
	ParamHandoff = "handoff"

	// StatePrefix starts the upstream state of a browser sign-in, so the
	// identity provider's one callback can tell it from an MCP client's
	// authorization. "." is not in the base64url alphabet of the
	// authorization's states.
	StatePrefix = "v."

	// ParamWith asks the sign-in page for the API-key form, with WithKey,
	// on an instance that also takes passwords.
	ParamWith = "with"
	WithKey   = "key"
)

// ActionSignIn is the audit action of a sign-in.
const ActionSignIn = "oauth.signin"

// How a person signed in, in the audit trail and the sign-in metric.
const (
	ViaOIDC     = "oidc"
	ViaAPIKey   = "api_key"
	ViaPassword = "password"
	ViaPasskey  = "passkey"
	// ViaLink is a person signed in by redeeming an invite, a reset or the
	// setup link.
	ViaLink = "link"
)

var (
	ErrNotFound = errors.New("signin: not found")
	ErrCodeUsed = errors.New("signin: code already used")
)

// Request is a browser sign-in at the identity provider, started for a
// host. Its ID is the upstream state.
type Request struct {
	ID               string
	Binding          string // hash of the browser's sign-in cookie
	Nonce            string
	UpstreamVerifier string
	Host             string // host label: <slug>, <slug>--v<N> or a control host
	Path             string // where to return to on that host
	NonceHash        string // S256 of the host's nonce cookie
	ExpiresAt        time.Time
}

// Session is a person signed in in one browser, held in a cookie on the
// MCP host. Only its hash is stored.
type Session struct {
	ID         int64
	Hash       string
	Browser    string // hash of the browser's sign-in cookie; "" links no sessions
	UserHandle string
	KeyPrefix  string // the API key that signed in; "" after a password or identity-provider sign-in
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// UserDisabled is whether the person is disabled now, read with the
	// session.
	UserDisabled bool
}

// ViewCode carries a session to one host. Only its hash is stored; it is
// single use.
type ViewCode struct {
	Hash      string
	SessionID int64
	Host      string
	Path      string
	NonceHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// ViewSession is a session's cookie on one host. Only its hash is stored.
// UserHandle, KeyPrefix and UserDisabled come from the session.
type ViewSession struct {
	ID           int64
	Hash         string
	SessionID    int64
	Host         string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	UserHandle   string
	KeyPrefix    string
	UserDisabled bool
}

// View is a redeemed code: the token of the new view session, when it
// ends, and the path on the host to return to.
type View struct {
	Token     string
	ExpiresAt time.Time
	Path      string
}

// ControlSession is a person signed in on a control host.
type ControlSession struct {
	Handle string
	// KeyPrefix is the API key the person signed in with; "" after a
	// password or an identity-provider sign-in.
	KeyPrefix string
	// SessionID is the sign-in session, which every host it opened shares.
	SessionID int64
	ExpiresAt time.Time
	// User is the person, as read to check the session.
	User *user.User
	// KeyScopes are what the API key acts with now, capped at the person's
	// role; nil without a key.
	KeyScopes []string
}

// Store is the persistence contract. Users are referenced by handle.
type Store interface {
	// CreateSignIn stores a pending sign-in at the identity provider.
	CreateSignIn(ctx context.Context, r *Request) error
	// GetSignIn returns a sign-in by id, expired or not. ErrNotFound.
	GetSignIn(ctx context.Context, id string) (*Request, error)
	// DeleteSignIn removes a sign-in. ErrNotFound when it is already gone,
	// so of two callbacks racing for one sign-in only one proceeds.
	DeleteSignIn(ctx context.Context, id string) error

	// CreateSession stores s and sets its ID. user.ErrNotFound for an
	// unknown handle.
	CreateSession(ctx context.Context, s *Session) error
	// GetSession returns a session by hash, expired or not. ErrNotFound.
	GetSession(ctx context.Context, hash string) (*Session, error)
	// GetSessionByID returns a session by id, expired or not. ErrNotFound.
	GetSessionByID(ctx context.Context, id int64) (*Session, error)
	// DeleteSession removes a session and every session with the same
	// non-empty Browser, with their codes and view sessions; a missing one
	// is not an error.
	DeleteSession(ctx context.Context, id int64) error
	// DeleteUserSessions removes the user's sessions, with their codes and
	// view sessions, and returns how many. It spares the session with id
	// keep and those of the same browser; 0 spares none.
	DeleteUserSessions(ctx context.Context, handle string, keep int64) (int, error)
	// CountSessions counts the user's sessions that have not expired at now.
	CountSessions(ctx context.Context, handle string, now time.Time) (int, error)

	// CreateViewCode stores a code for an existing session.
	CreateViewCode(ctx context.Context, c *ViewCode) error
	// ConsumeViewCode atomically marks the code used at at and returns it.
	// ErrNotFound for an unknown code, ErrCodeUsed for a second use.
	ConsumeViewCode(ctx context.Context, hash string, at time.Time) (*ViewCode, error)

	// CreateViewSession stores v for an existing session and sets its ID.
	CreateViewSession(ctx context.Context, v *ViewSession) error
	// GetViewSession returns a view session by hash, expired or not, with
	// its session's user and key prefix. ErrNotFound.
	GetViewSession(ctx context.Context, hash string) (*ViewSession, error)
	// DeleteViewSession removes one view session and leaves its session; a
	// missing view session is not an error.
	DeleteViewSession(ctx context.Context, hash string) error

	// DeleteExpired returns the number of sign-ins, codes, view sessions
	// and sessions removed.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}

// Users are the people who sign in. user.Service implements it.
type Users interface {
	// ActiveUser returns user.ErrNotFound for a person who is unknown or
	// disabled.
	ActiveUser(ctx context.Context, handle string) (*user.User, error)
	// AccountKey is what failed password sign-ins of login count under.
	AccountKey(ctx context.Context, login string) string
	// ResolveIdentity returns user.ErrNotProvisioned or user.ErrDisabled
	// for a person who may not sign in.
	ResolveIdentity(ctx context.Context, id auth.Identity) (*user.User, error)
}

// Keys checks the API keys people sign in with, at sign-in and on every
// later use of the session. user.Service implements it.
type Keys interface {
	auth.Authenticator
	// KeyStatus returns user.ErrKeyNotFound for an unknown prefix.
	KeyStatus(ctx context.Context, prefix string) (user.KeyStatus, error)
}

// Passwords signs people in to the accounts Kazari keeps itself.
// user.Service implements it.
type Passwords interface {
	// CheckPassword returns the person whose handle or email login is, if
	// the password is theirs, and user.ErrBadPassword for every refusal.
	CheckPassword(ctx context.Context, login, password string) (*user.User, error)
}

// Passkeys signs people in to the accounts Kazari keeps itself with a
// passkey. passkey.Service implements it.
type Passkeys interface {
	// SignIn begins a sign-in, for a page to carry.
	SignIn() (passkey.Ceremony, error)
	// Verify returns the person whose passkey answered the ceremony token
	// names: passkey.ErrExpired for a token that is not live,
	// passkey.ErrRejected for an answer that is not accepted.
	Verify(ctx context.Context, token, response string) (*user.User, error)
}

// IdentityProvider sends a browser to sign in and turns the callback into
// an Identity. One that also has a Ready method that returns an error
// cannot sign anyone in yet, and the sign-in pages say so.
type IdentityProvider interface {
	// AuthURL takes the S256 PKCE challenge.
	AuthURL(state, nonce, codeChallenge string) string
	// Exchange redeems the callback code and verifies the ID token.
	Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error)
}

// Deps are the collaborators of the sign-in host. Keys, Passwords,
// Passkeys, IdP and Audit may be nil. With an identity provider, Passwords
// and Passkeys are not used; Passkeys is used only with Passwords.
type Deps struct {
	Users     Users
	Keys      Keys
	Passwords Passwords
	Passkeys  Passkeys
	IdP       IdentityProvider
	Audit     audit.Recorder
	// ViewCallback returns the URL on an artifact host where a browser
	// sign-in ends (https://demo.art.example.com/.kazari/callback), given
	// the host's label, and whether the label names an artifact host. Nil
	// signs in for no artifact host.
	ViewCallback func(host string) (string, bool)
	// ControlCallback is ViewCallback for the control hosts, HostAdmin and
	// HostAccount: the URL their sign-in ends at, built from the
	// configuration and never from a request.
	ControlCallback func(host string) (string, bool)
}

// Options configures a Service.
type Options struct {
	// Origin is the MCP host's, e.g. https://mcp.art.example.com: forms
	// must come from it.
	Origin string
	// RecoverURL is the page that sets a new password with a recovery code,
	// which the password form links to; "" leaves the link out.
	RecoverURL string
	// SessionTTL is how long a browser sign-in lasts (default 12h); every
	// view session it opens ends with it. ViewCodeTTL is the lifetime of
	// the code that carries it to a host (default 1m), RequestTTL that of a
	// sign-in at the identity provider (default 10m).
	SessionTTL, ViewCodeTTL, RequestTTL time.Duration
	// InsecureCookies drops the Secure attribute and the __Host- prefix, for
	// development over plain http.
	InsecureCookies bool
	Now             func() time.Time
	Logger          *slog.Logger
	Telemetry       instrument.Providers
}

const (
	DefaultSessionTTL  = 12 * time.Hour
	DefaultViewCodeTTL = time.Minute
	DefaultRequestTTL  = 10 * time.Minute
)

// How failed password sign-ins slow the next: three wrong passwords for an
// account, or ten from one address, which may be a whole office, cost
// nothing; then the wait doubles from a second up to a minute. Nobody is
// ever locked out.
const (
	accountFreeFailures = 3
	ipFreeFailures      = 10
	signInBaseDelay     = time.Second
	signInMaxDelay      = time.Minute
)

// Service is the sign-in host.
type Service struct {
	store     Store
	users     Users
	keys      Keys
	passwords Passwords // nil with an identity provider
	passkeys  Passkeys  // nil without passwords
	idp       IdentityProvider
	audit     audit.Recorder
	view      func(host string) (string, bool)
	control   func(host string) (string, bool)
	opts      Options
	now       func() time.Time
	log       *slog.Logger
	cookies   cookie.Host
	pages     *Pages
	tracer    trace.Tracer
	signIns   signInCounter

	// Failed password sign-ins slow the next one down, per account and per
	// client address.
	accountDelay, ipDelay *ratelimit.Backoff
}

// New returns a Service.
func New(store Store, d Deps, opts Options) *Service {
	if d.Audit == nil {
		d.Audit = audit.Nop{}
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	if opts.ViewCodeTTL <= 0 {
		opts.ViewCodeTTL = DefaultViewCodeTTL
	}
	if opts.RequestTTL <= 0 {
		opts.RequestTTL = DefaultRequestTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// Built-in accounts or an identity provider, never both.
	passwords, passkeys := d.Passwords, d.Passkeys
	if d.IdP != nil {
		passwords = nil
	}
	if passwords == nil {
		passkeys = nil
	}
	backoff := func(free int, scope string) *ratelimit.Backoff {
		return ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: free, Base: signInBaseDelay, Max: signInMaxDelay,
			Now: opts.Now, Scope: scope, Telemetry: opts.Telemetry})
	}
	return &Service{
		store: store, users: d.Users, keys: d.Keys, passwords: passwords, passkeys: passkeys, idp: d.IdP, audit: d.Audit,
		view: d.ViewCallback, control: d.ControlCallback,
		opts: opts, now: opts.Now, log: opts.Logger, cookies: cookie.Host{Insecure: opts.InsecureCookies},
		pages: NewPages(opts.Origin, nil), tracer: opts.Telemetry.Tracer(scope), signIns: newSignInCounter(opts.Telemetry.Meter(scope)),
		accountDelay: backoff(accountFreeFailures, ratelimit.ScopeAccount), ipDelay: backoff(ipFreeFailures, ratelimit.ScopeIP),
	}
}

// Passwords reports whether people sign in with the password of a built-in
// account.
func (s *Service) Passwords() bool { return s.passwords != nil }

// Redeem exchanges a code from a sign-in redirect for a view session on
// host. nonce is the value of the host's nonce cookie, whose S256 the code
// carries, so a code only works in the browser that asked for it. The view
// session ends with its sign-in session. ErrNotFound for an unknown, used,
// expired or foreign code, a nonce that does not match, or a session that
// is no longer live; the code is used up in every case. A missing or
// malformed nonce never matches: a code made for the S256 of "" would
// otherwise sign in any browser that has no nonce cookie, as whoever made
// it.
func (s *Service) Redeem(ctx context.Context, code, host, nonce string) (_ View, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "oauth.redeem")
	defer func() { instrument.End(span, err, redeemCode(err)) }()
	if len(code) != tokenLen {
		return View{}, ErrNotFound
	}
	c, err := s.store.ConsumeViewCode(ctx, hashSecret(code), s.now())
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCodeUsed) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("signin: redeem: %w", err)
	}
	if !s.now().Before(c.ExpiresAt) || c.Host != host || !validNonce(nonce) ||
		subtle.ConstantTimeCompare([]byte(s256(nonce)), []byte(c.NonceHash)) != 1 {
		return View{}, ErrNotFound
	}
	sess, err := s.store.GetSessionByID(ctx, c.SessionID)
	if errors.Is(err, ErrNotFound) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("signin: redeem: %w", err)
	}
	key, err := s.live(ctx, sess.UserHandle, sess.KeyPrefix, sess.ExpiresAt, sess.UserDisabled)
	if err != nil {
		return View{}, fmt.Errorf("signin: redeem: %w", err)
	}
	if !key.live {
		return View{}, ErrNotFound
	}
	token := randomString(secretBytes)
	v := &ViewSession{Hash: hashSecret(token), SessionID: sess.ID, Host: host, CreatedAt: s.now(), ExpiresAt: sess.ExpiresAt}
	if err := s.store.CreateViewSession(ctx, v); err != nil {
		return View{}, fmt.Errorf("signin: redeem: %w", err)
	}
	return View{Token: token, ExpiresAt: v.ExpiresAt, Path: c.Path}, nil
}

// hostHandoff is the label of a LinkCode's code: the sign-in host itself,
// which no host's Redeem names.
const hostHandoff = "@signin"

// LinkCode is a single-use code that signs handle in at the sign-in host,
// for a person a host has just identified: they redeemed a link and chose a
// password. The host sends the browser to the sign-in page with it as the
// handoff parameter; the sign-in host then opens the browser's one sign-in
// session there, as a password would, and the browser goes on to the host
// as after any sign-in. Every other host joins that session, and signing
// out anywhere ends it.
func (s *Service) LinkCode(ctx context.Context, handle string) (string, error) {
	now := s.now()
	expires := now.Add(s.opts.ViewCodeTTL)
	// A code belongs to a session; this one holds the person until the
	// code is used, and opens nothing itself.
	anchor := &Session{Hash: hashSecret(randomString(secretBytes)), UserHandle: handle, CreatedAt: now, ExpiresAt: expires}
	if err := s.store.CreateSession(ctx, anchor); err != nil {
		return "", fmt.Errorf("signin: link code: %w", err)
	}
	code := randomString(secretBytes)
	if err := s.store.CreateViewCode(ctx, &ViewCode{Hash: hashSecret(code), SessionID: anchor.ID, Host: hostHandoff, ExpiresAt: expires}); err != nil {
		return "", fmt.Errorf("signin: link code: %w", err)
	}
	return code, nil
}

// Viewer returns the handle of the person whose view session on host
// token is. ErrNotFound unless the view session exists for that host, has
// not expired and its sign-in session is still live.
func (s *Service) Viewer(ctx context.Context, host, token string) (string, error) {
	v, err := s.viewSession(ctx, host, token)
	if err != nil {
		return "", err
	}
	key, err := s.live(ctx, v.UserHandle, v.KeyPrefix, v.ExpiresAt, v.UserDisabled)
	if err != nil {
		return "", fmt.Errorf("signin: viewer: %w", err)
	}
	if !key.live {
		return "", ErrNotFound
	}
	return v.UserHandle, nil
}

// ControlSession returns who the view session token on the control host is,
// and how they signed in. ErrNotFound unless the view session exists for
// that host, has not expired and its sign-in session is still live: the
// person enabled and, for an API-key sign-in, the key still active. What
// the person may do there is the host's to decide, on every request.
func (s *Service) ControlSession(ctx context.Context, host, token string) (ControlSession, error) {
	v, err := s.viewSession(ctx, host, token)
	if err != nil {
		return ControlSession{}, err
	}
	key, err := s.live(ctx, v.UserHandle, v.KeyPrefix, v.ExpiresAt, v.UserDisabled)
	if err != nil {
		return ControlSession{}, fmt.Errorf("signin: control session: %w", err)
	}
	if !key.live {
		return ControlSession{}, ErrNotFound
	}
	u, err := s.users.ActiveUser(ctx, v.UserHandle)
	if errors.Is(err, user.ErrNotFound) {
		return ControlSession{}, ErrNotFound
	}
	if err != nil {
		return ControlSession{}, fmt.Errorf("signin: control session: %w", err)
	}
	return ControlSession{Handle: v.UserHandle, KeyPrefix: v.KeyPrefix, SessionID: v.SessionID, ExpiresAt: v.ExpiresAt,
		User: u, KeyScopes: key.scopes}, nil
}

// SignOut ends the sign-in behind the view session token on the control
// host: the session, every session of the same browser and every view
// session they opened, on every host. An unknown token is not an error.
func (s *Service) SignOut(ctx context.Context, host, token string) error {
	v, err := s.viewSession(ctx, host, token)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.DeleteSession(ctx, v.SessionID)
}

// EndView ends the view session token on host. Its sign-in session and
// the view sessions of other hosts stay. An unknown token is not an error.
func (s *Service) EndView(ctx context.Context, host, token string) error {
	v, err := s.viewSession(ctx, host, token)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.DeleteViewSession(ctx, v.Hash)
}

// EndSessions ends the user's browser sessions on every host and leaves
// their tokens alone, so the applications they connected keep working. The
// session with id keep, where a person changed their own password, stays;
// 0 keeps none.
func (s *Service) EndSessions(ctx context.Context, handle string, keep int64) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "oauth.end_sessions")
	defer func() { instrument.End(span, err, "") }()
	n, err := s.store.DeleteUserSessions(ctx, handle, keep)
	if err != nil {
		return err
	}
	if n > 0 {
		s.record(ctx, actorFrom(ctx, handle), audit.SessionRevoke, handle, map[string]string{"user": handle, "sessions": strconv.Itoa(n)})
	}
	return nil
}

// CountSessions returns how many browser sessions the user has.
func (s *Service) CountSessions(ctx context.Context, handle string) (int, error) {
	return s.store.CountSessions(ctx, handle, s.now())
}

// Cleanup deletes expired sign-ins, codes and sessions, returning the
// count.
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	return s.store.DeleteExpired(ctx, s.now())
}

// viewSession loads the view session token names on host. ErrNotFound.
func (s *Service) viewSession(ctx context.Context, host, token string) (*ViewSession, error) {
	if len(token) != tokenLen {
		return nil, ErrNotFound
	}
	v, err := s.store.GetViewSession(ctx, hashSecret(token))
	if err != nil {
		return nil, err
	}
	if v.Host != host {
		return nil, ErrNotFound
	}
	return v, nil
}

// keyState is what a session opened with an API key takes from it.
type keyState struct {
	live    bool       // the session may be used: see live
	expires *time.Time // nil when the key never expires
	scopes  []string   // what the key acts with now
}

// live is the check every use of a session or view session makes: it has
// not expired, its person is not disabled and, for an API-key sign-in, the
// key is still active, the person's own and unrestricted.
func (s *Service) live(ctx context.Context, handle, keyPrefix string, expires time.Time, disabled bool) (keyState, error) {
	if !s.now().Before(expires) || disabled {
		return keyState{}, nil
	}
	return s.key(ctx, keyPrefix, handle)
}

// key reads the API key a session was opened with. Prefix "" (a password
// or identity-provider sign-in) is live and unbounded; an unknown,
// inactive or restricted key, or one that is not handle's, is not live.
func (s *Service) key(ctx context.Context, prefix, handle string) (keyState, error) {
	if prefix == "" {
		return keyState{live: true}, nil
	}
	if s.keys == nil {
		return keyState{}, nil
	}
	st, err := s.keys.KeyStatus(ctx, prefix)
	if errors.Is(err, user.ErrKeyNotFound) {
		return keyState{}, nil
	}
	if err != nil {
		return keyState{}, err
	}
	if !st.Active || st.Handle != handle || len(st.Slugs) > 0 {
		return keyState{}, nil
	}
	return keyState{live: true, expires: st.ExpiresAt, scopes: st.Scopes}, nil
}

// record appends an event about the account of subject.
func (s *Service) record(ctx context.Context, actor, action, subject string, details map[string]string) {
	// A lost audit row must not fail the request.
	if err := s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Subject: subject, Details: details}); err != nil {
		s.log.WarnContext(ctx, "signin: audit", "action", action, "err", err)
	}
}

func actorFrom(ctx context.Context, fallback string) string {
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		return p.Handle
	}
	return fallback
}

// redeemCode keeps an unknown or expired code, a stale browser tab, out of
// the internal errors.
func redeemCode(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "not_found"
	}
	return ""
}

// ReturnPath returns p if it is a path on the same host ("/" not followed
// by a second "/", with no backslash or control character anywhere, at
// most 2048 bytes), else "/". It keeps a return path from naming another
// host. A backslash is refused wherever it is, because browsers read it as
// "/" and cleaning "/x/../\h" leaves "/\h"; a browser sends one as %5C, so
// no real path holds one.
func ReturnPath(p string) string {
	if p == "" || p[0] != '/' || len(p) > maxReturnPath || strings.HasPrefix(p, "//") {
		return "/"
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f || p[i] == '\\' {
			return "/"
		}
	}
	return p
}

// maxReturnPath bounds the path a host asks to return to.
const maxReturnPath = 2048

// nonceLen is the length of a host's nonce cookie and of its S256 hash:
// 32 bytes, base64url.
const nonceLen = 43

// nonceValue matches a nonce and its hash, and a request id.
var nonceValue = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func validNonce(v string) bool { return len(v) == nonceLen && nonceValue.MatchString(v) }

const secretBytes = 32

var tokenLen = base64.RawURLEncoding.EncodedLen(secretBytes)

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand panics rather than fail
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// s256 is the PKCE (RFC 7636) S256 hash of v.
func s256(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// withQuery returns raw with v added to its query.
func withQuery(raw string, v url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	maps.Copy(q, v)
	u.RawQuery = q.Encode()
	return u.String()
}
