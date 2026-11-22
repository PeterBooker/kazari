// Package oauth is the OAuth 2.1 authorization server for MCP clients: RFC
// 8414 and RFC 9728 metadata, dynamic registration (RFC 7591), client ID
// metadata documents, PKCE (S256 only) and refresh-token rotation. People sign
// in at an upstream IdentityProvider or, without one, with the password of a
// built-in account or an API key; a grant approved with a key takes that
// key's scopes, restriction and lifetime. The admin scope is never granted.
//
// It is also the browser sign-in host for every other host: a session cookie
// on the MCP host, and per host a single-use code that the host redeems
// in-process (Redeem) for a cookie only it receives. Those hosts are the
// artifact hosts with gated pages and the control hosts, the admin host and
// the apex (HostAdmin, HostAccount).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

const (
	ActionGrant  = "oauth.grant"
	ActionRevoke = "oauth.revoke"
	ActionSignIn = "oauth.signin"
)

// ReasonRefreshReplay is the reason of an ActionRevoke that the server made
// itself, because a rotated refresh token was presented again.
const ReasonRefreshReplay = "refresh_replay"

var (
	ErrNotFound = errors.New("oauth: not found")
	ErrCodeUsed = errors.New("oauth: authorization code already used")
	// ErrClientLimit means a new client was refused because
	// Options.MaxUnusedClients clients are without a grant.
	ErrClientLimit = errors.New("oauth: too many clients without a grant")
)

// Client is a registered (DCR) or fetched (CIMD) public client.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
	FetchedAt    *time.Time // CIMD cache time, nil for DCR clients
	GrantedAt    *time.Time // when the client first completed a grant; nil until then
}

// Request is a pending authorization request.
type Request struct {
	ID               string
	ClientID         string
	RedirectURI      string
	Scopes           []string
	CodeChallenge    string
	Resource         string
	ClientState      string
	Nonce            string
	UpstreamState    string
	UpstreamVerifier string
	UserHandle       string // bound at callback; "" until then
	ExpiresAt        time.Time
}

// Code is an issued authorization code. Only its hash is stored.
type Code struct {
	Hash          string
	ClientID      string
	UserHandle    string
	Scopes        []string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	KeyPrefix     string // the API key that approved it; "" after an identity-provider sign-in
	ExpiresAt     time.Time
	UsedAt        *time.Time
	FamilyID      int64 // the token family it was exchanged for; 0 until then
}

// Token is an issued access or refresh token. Only its hash is stored.
type Token struct {
	ID         int64
	Hash       string
	Kind       string // KindAccess | KindRefresh
	ClientID   string
	UserHandle string
	Scopes     []string
	Resource   string
	KeyPrefix  string // the API key that approved the grant; "" after an identity-provider sign-in
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *int64 // refresh tokens: the refresh token that replaced this one
	FamilyID   int64  // id of the first refresh token of the chain
}

// SignInRequest is a browser sign-in at the identity provider, started for
// an artifact host. Its ID is the upstream state.
type SignInRequest struct {
	ID               string
	Binding          string // hash of the browser's sign-in cookie
	Nonce            string
	UpstreamVerifier string
	Host             string // artifact host label: <slug> or <slug>--v<N>
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
	KeyPrefix  string // the API key that signed in; "" after an identity-provider sign-in
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// ViewCode carries a session to one artifact host. Only its hash is
// stored; it is single use.
type ViewCode struct {
	Hash      string
	SessionID int64
	Host      string
	Path      string
	NonceHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// ViewSession is a session's cookie on one artifact host. Only its hash is
// stored. UserHandle and KeyPrefix come from the session.
type ViewSession struct {
	ID         int64
	Hash       string
	SessionID  int64
	Host       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UserHandle string
	KeyPrefix  string
}

// Store is the persistence contract. Users are referenced by handle.
type Store interface {
	// UpsertClient inserts c or replaces its name, redirect URIs and fetch
	// time, never GrantedAt.
	UpsertClient(ctx context.Context, c *Client) error
	// AddClient is UpsertClient for a client that anyone can create: when
	// maxUnused clients have no grant it stores nothing and returns
	// ErrClientLimit, atomically, so concurrent calls cannot pass the limit.
	AddClient(ctx context.Context, c *Client, maxUnused int) error
	// GetClient returns ErrNotFound for an unknown id.
	GetClient(ctx context.Context, id string) (*Client, error)
	// DeleteUnusedClients removes clients created before the given time
	// that never completed a grant and have no pending request or code,
	// and returns how many.
	DeleteUnusedClients(ctx context.Context, before time.Time) (int, error)

	CreateRequest(ctx context.Context, r *Request) error
	// GetRequest returns a request, expired or not, or ErrNotFound.
	GetRequest(ctx context.Context, id string) (*Request, error)
	// BindRequest returns ErrNotFound when the request is missing or bound.
	BindRequest(ctx context.Context, id, handle string) error
	// DeleteRequest of a missing request is not an error.
	DeleteRequest(ctx context.Context, id string) error

	CreateCode(ctx context.Context, c *Code) error
	// ConsumeCode atomically marks the code used at at and returns it.
	// ErrNotFound for an unknown code; ErrCodeUsed for a second use, still
	// with the code, so the caller can revoke what the first use obtained.
	ConsumeCode(ctx context.Context, hash string, at time.Time) (*Code, error)
	// LinkCodeFamily records the token family a consumed code was
	// exchanged for. ErrNotFound.
	LinkCodeFamily(ctx context.Context, hash string, familyID int64) error

	// CreateTokens stores a pair that starts a new family, setting both IDs
	// and FamilyID (the refresh token's id), and marks the client granted.
	CreateTokens(ctx context.Context, access, refresh *Token) error
	// GetToken returns revoked and expired tokens too, or ErrNotFound.
	GetToken(ctx context.Context, hash string) (*Token, error)
	// Rotate atomically revokes the live refresh token oldID, stores access
	// and refresh in its family and records refresh as its replacement.
	// ErrNotFound when oldID is missing, revoked or already rotated.
	Rotate(ctx context.Context, oldID int64, at time.Time, access, refresh *Token) error
	// Reissue stores another pair in the family of the rotated refresh token
	// oldID, for a client that never got the answer to its refresh, without
	// touching the pair that answer held. It does so only while the family
	// is exactly as that rotation left it: oldID was rotated after since,
	// and its successor is live (at refresh.CreatedAt) and still the newest
	// refresh token of the family, so it was neither used nor has oldID
	// been reissued for before. ErrNotFound otherwise, with nothing stored.
	Reissue(ctx context.Context, oldID int64, since time.Time, access, refresh *Token) error
	RevokeFamily(ctx context.Context, familyID int64, at time.Time) error
	// ListGrants returns the newest live refresh token of each of the
	// user's families, newest first. Its FamilyID names the grant.
	ListGrants(ctx context.Context, handle string, now time.Time) ([]Token, error)
	// RevokeGrant revokes every token of the family and returns the handle
	// of the person it belongs to. ErrNotFound for a
	// family that never held a refresh token.
	RevokeGrant(ctx context.Context, familyID int64, at time.Time) (handle string, err error)
	// RevokeAllForUser also deletes the user's sessions.
	RevokeAllForUser(ctx context.Context, handle string, at time.Time) error
	// DeleteExpired returns the number of requests, codes, tokens, sign-ins
	// and sessions removed.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)

	// CreateSignIn stores a pending browser sign-in.
	CreateSignIn(ctx context.Context, r *SignInRequest) error
	// GetSignIn returns a sign-in by id, expired or not. ErrNotFound.
	GetSignIn(ctx context.Context, id string) (*SignInRequest, error)
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
	// keep and those of the same browser; 0 spares none. The user's tokens
	// stay.
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

// idpUnavailable reports, and answers, a sign-in the identity provider
// cannot take yet, such as before it has been reached once.
func (s *Service) idpUnavailable(w http.ResponseWriter, r *http.Request) bool {
	p, ok := s.idp.(interface{ Ready() error })
	if !ok {
		return false
	}
	err := p.Ready()
	if err == nil {
		return false
	}
	s.log.WarnContext(r.Context(), "oauth: sign-in while the identity provider is unavailable", "err", err)
	w.Header().Set("Retry-After", "30")
	s.message(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable",
		"This instance cannot reach its identity provider yet. Please try again in a minute.", "")
	return true
}

// Keys checks the API keys that grants and browser sessions are approved
// with, at consent and on every later use. user.Service implements it.
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

// Options configures a Service.
type Options struct {
	Issuer string // e.g. https://mcp.art.example.com
	// Resource is the MCP URL as clients enter it, e.g.
	// https://mcp.art.example.com/.
	Resource string
	// Zero means the Default*TTL. ClientTTL is how long a registered or
	// fetched client that completes no grant is kept.
	AccessTTL, RefreshTTL, RequestTTL, CodeTTL, ClientTTL time.Duration
	// RefreshReuseWindow is how long after its rotation a refresh token is
	// accepted once more, for a client that never got the answer to its
	// refresh: it gets another pair in the same family. 0 never accepts a
	// rotated token again.
	RefreshReuseWindow time.Duration
	// MaxUnusedClients is how many clients without a grant may exist before
	// new ones are refused (default DefaultMaxUnusedClients).
	MaxUnusedClients int
	// Scopes offered to clients (default auth.MemberScopes). auth.ScopeAdmin
	// is dropped so a client an admin approves cannot open the admin host.
	Scopes []string
	Now    func() time.Time
	Logger *slog.Logger
	// Fetch records the fetches of client ID metadata documents; nil records
	// nothing.
	Fetch     *fetch.Observer
	Telemetry instrument.Providers
	// HTTPClient fetches client ID metadata documents. The default times out
	// after 10 s and dials public addresses only.
	HTTPClient *http.Client
	// InsecureCookies drops the Secure attribute and the __Host- prefix, for
	// development over plain http.
	InsecureCookies bool
	// Passwords signs people in with a built-in account. Nil, or an
	// identity provider, turns the password form off; the pages then ask
	// for an API key.
	Passwords Passwords
	// Passkeys adds a passkey sign-in beside the password form. It is used
	// only with Passwords.
	Passkeys Passkeys
	// RecoverURL is the page that sets a new password with a recovery code,
	// which the password form links to; "" leaves the link out.
	RecoverURL string
	// ViewCallback returns the URL on an artifact host where a browser
	// sign-in ends (https://demo.art.example.com/.kazari/callback), given
	// the host's label, and whether the label names an artifact host. Nil
	// turns browser sign-in off.
	ViewCallback func(host string) (string, bool)
	// ControlCallback is ViewCallback for the control hosts, HostAdmin and
	// HostAccount: the URL their sign-in ends at, built from the
	// configuration and never from a request.
	ControlCallback func(host string) (string, bool)
	// SessionTTL is how long a browser sign-in lasts (default 12h); every
	// view session it opens ends with it. ViewCodeTTL is the lifetime of
	// the code that carries it to an artifact host (default 1m).
	SessionTTL, ViewCodeTTL time.Duration
}

const (
	DefaultAccessTTL   = time.Hour
	DefaultRefreshTTL  = 90 * 24 * time.Hour
	DefaultRequestTTL  = 10 * time.Minute
	DefaultCodeTTL     = 5 * time.Minute
	DefaultSessionTTL  = 12 * time.Hour
	DefaultViewCodeTTL = time.Minute
	DefaultClientTTL   = 24 * time.Hour
)

// DefaultMaxUnusedClients with maxRedirectURIs of maxRedirectURILen bounds
// what anonymous callers can keep in the database to about 400 MiB of redirect
// URIs, or 2.4 GiB at worst, because their stored JSON escapes & < > to six
// bytes.
const DefaultMaxUnusedClients = 10_000

// Service is the authorization server.
type Service struct {
	store     Store
	users     *user.Service
	keys      Keys
	passwords Passwords // nil with an identity provider
	passkeys  Passkeys  // nil without passwords
	idp       IdentityProvider
	audit     audit.Recorder
	opts      Options
	origin    string // of the issuer
	now       func() time.Time
	log       *slog.Logger
	client    *http.Client
	pages     *pages
	tracer    trace.Tracer
	m         metrics

	// Failed password sign-ins slow the next one down, per account and per
	// client address.
	accountDelay, ipDelay *ratelimit.Backoff
}

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

var _ auth.Authenticator = (*Service)(nil)

// New returns a Service. keys checks the consent page's API key; keys, idp
// and rec may be nil, but without keys grants approved with a key stop
// working.
func New(store Store, users *user.Service, keys Keys, idp IdentityProvider, rec audit.Recorder, opts Options) *Service {
	if rec == nil {
		rec = audit.Nop{}
	}
	opts.Issuer = strings.TrimSuffix(opts.Issuer, "/")
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = DefaultAccessTTL
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = DefaultRefreshTTL
	}
	if opts.RequestTTL <= 0 {
		opts.RequestTTL = DefaultRequestTTL
	}
	if opts.CodeTTL <= 0 {
		opts.CodeTTL = DefaultCodeTTL
	}
	if opts.ClientTTL <= 0 {
		opts.ClientTTL = DefaultClientTTL
	}
	if opts.MaxUnusedClients <= 0 {
		opts.MaxUnusedClients = DefaultMaxUnusedClients
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	if opts.ViewCodeTTL <= 0 {
		opts.ViewCodeTTL = DefaultViewCodeTTL
	}
	opts.Scopes = slices.DeleteFunc(slices.Clone(opts.Scopes), func(sc string) bool { return sc == auth.ScopeAdmin })
	if len(opts.Scopes) == 0 {
		opts.Scopes = slices.Clone(auth.MemberScopes)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: cimdTimeout, Transport: opts.Fetch.Transport(fetch.Transport())}
	}
	var origin string
	if u, err := url.Parse(opts.Issuer); err == nil && u.Host != "" {
		origin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	// Built-in accounts or an identity provider, never both.
	passwords, passkeys := opts.Passwords, opts.Passkeys
	if idp != nil {
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
		store: store, users: users, keys: keys, passwords: passwords, passkeys: passkeys, idp: idp, audit: rec, opts: opts, origin: origin,
		now: opts.Now, log: opts.Logger, client: opts.HTTPClient, pages: newPages(origin),
		tracer: opts.Telemetry.Tracer(scope), m: newMetrics(opts.Telemetry.Meter(scope)),
		accountDelay: backoff(accountFreeFailures, ratelimit.ScopeAccount), ipDelay: backoff(ipFreeFailures, ratelimit.ScopeIP),
	}
}

// Authenticate resolves an access token. Unknown, revoked or expired tokens,
// disabled users and grants whose approving API key is no longer active
// yield auth.ErrInvalidToken, so auth.Chain moves on. Scopes are capped at
// the offered scopes and the user's current role, never admin. KeyID names
// the grant, so it is stable across refreshes.
func (s *Service) Authenticate(ctx context.Context, token string) (auth.Principal, error) {
	if len(token) != tokenLen {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	t, err := s.store.GetToken(ctx, hashSecret(token))
	if errors.Is(err, ErrNotFound) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
	}
	if t.Kind != KindAccess || t.RevokedAt != nil || !s.now().Before(t.ExpiresAt) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	u, err := s.users.GetUser(ctx, t.UserHandle)
	if errors.Is(err, user.ErrNotFound) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
	}
	if u.DisabledAt != nil {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	key, err := s.approvingKey(ctx, t.KeyPrefix, t.UserHandle)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
	}
	if !key.live {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	// Capping at Options.Scopes also strips admin from tokens issued
	// before admin was dropped. The restriction is read from the key on
	// every request, so the grant is never wider than the key is now.
	return auth.Principal{
		Handle:     u.Handle,
		Scopes:     intersect(intersect(t.Scopes, s.opts.Scopes), user.ScopesFor(u)),
		Source:     auth.SourceOAuth,
		KeyID:      fmt.Sprintf("oauth:%d", t.FamilyID),
		QuotaBytes: u.QuotaBytes,
		ClientID:   t.ClientID,
		Slugs:      key.slugs,
		ExpiresAt:  key.expires,
	}, nil
}

// Recognize implements auth.Recognizer for access and refresh tokens.
func (s *Service) Recognize(token string) (string, bool) {
	if len(token) != tokenLen {
		return "", false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return auth.SourceOAuth, err == nil
}

// keyState is what a credential derived from an API key takes from it.
type keyState struct {
	live    bool       // the key is active and belongs to the credential's user
	expires *time.Time // nil when it never expires
	slugs   []string   // its restriction, empty when it has none
}

// approvingKey reads the key a grant or session was approved with. Prefix ""
// (an identity-provider sign-in) is live and unbounded; an unknown or
// inactive key, or one that is not handle's, is not live.
func (s *Service) approvingKey(ctx context.Context, prefix, handle string) (keyState, error) {
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
	if !st.Active || st.Handle != handle {
		return keyState{}, nil
	}
	return keyState{live: true, expires: st.ExpiresAt, slugs: st.Slugs}, nil
}

// GetClient returns a registered or cached client, or ErrNotFound.
func (s *Service) GetClient(ctx context.Context, id string) (*Client, error) {
	return s.store.GetClient(ctx, id)
}

// ListGrants returns the user's grants, newest first, each as its newest
// live refresh token: CreatedAt is the last refresh, ExpiresAt when the
// grant lapses unless refreshed again, and FamilyID its id, the number in
// the "oauth:<id>" credential of the audit log.
func (s *Service) ListGrants(ctx context.Context, handle string) ([]Token, error) {
	return s.store.ListGrants(ctx, handle, s.now())
}

// RevokeGrant revokes every token of the grant with that id. ErrNotFound.
func (s *Service) RevokeGrant(ctx context.Context, id int64) error {
	handle, err := s.store.RevokeGrant(ctx, id, s.now())
	if err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, handle, map[string]string{"grant": fmt.Sprint(id)})
	return nil
}

// RevokeAllForUser revokes every token of the user. It is wired to
// user.Options.OnDisable, so re-enabling a user does not restore them.
func (s *Service) RevokeAllForUser(ctx context.Context, handle string) error {
	if err := s.store.RevokeAllForUser(ctx, handle, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, handle, map[string]string{"user": handle})
	return nil
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

// Cleanup deletes expired requests, codes, tokens and sign-ins, and clients
// that completed no grant within ClientTTL, returning the count.
func (s *Service) Cleanup(ctx context.Context) (n int, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "oauth.cleanup")
	defer func() { instrument.End(span, err, "") }()
	now := s.now()
	n, err = s.store.DeleteExpired(ctx, now)
	if err != nil {
		return n, err
	}
	clients, err := s.store.DeleteUnusedClients(ctx, now.Add(-s.opts.ClientTTL))
	return n + clients, err
}

// record appends an event about the account of subject.
func (s *Service) record(ctx context.Context, actor, action, subject string, details map[string]string) {
	// A lost audit row must not fail the request.
	if err := s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Subject: subject, Details: details}); err != nil {
		s.log.WarnContext(ctx, "oauth: audit", "action", action, "err", err)
	}
}

func actorFrom(ctx context.Context, fallback string) string {
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		return p.Handle
	}
	return fallback
}

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

// s256 is the PKCE (RFC 7636) S256 challenge of verifier.
func s256(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// intersect returns the members of a that are in b, in a's order.
func intersect(a, b []string) []string {
	out := []string{}
	for _, s := range a {
		if slices.Contains(b, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
