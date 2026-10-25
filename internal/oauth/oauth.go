// Package oauth is the OAuth 2.1 authorization server for MCP clients and
// the sign-in host for people opening gated artifacts in a browser. It
// serves the RFC 8414 and RFC 9728 metadata documents, dynamic client
// registration (RFC 7591), client ID metadata documents, the authorization
// code flow with PKCE (S256 only) and refresh-token rotation.
//
// People sign in at an upstream IdentityProvider (an OpenID Connect provider,
// see package oidc); without one, the consent page asks for an API key, and
// the grant then lives only as long as that key. The admin scope is never
// offered or granted. Access and refresh tokens are opaque random strings
// stored as SHA-256 hashes. Service implements auth.Authenticator for access
// tokens.
//
// Browser sign-in (signin.go) keeps a session cookie on the MCP host and
// hands each artifact host a single-use code, which the host redeems
// in-process (Redeem) for a view session of its own: a cookie that only
// that host receives.
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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"kazari/internal/audit"
	"kazari/internal/auth"
	"kazari/internal/user"
)

// Token kinds.
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// Audit actions recorded by the service.
const (
	ActionGrant  = "oauth.grant"
	ActionRevoke = "oauth.revoke"
	ActionSignIn = "oauth.signin"
)

// Errors returned by a Store. Compare with errors.Is.
var (
	ErrNotFound = errors.New("oauth: not found")
	ErrCodeUsed = errors.New("oauth: authorization code already used")
)

// Client is a registered (DCR) or fetched (CIMD) public client.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
	FetchedAt    *time.Time // CIMD cache time, nil for DCR clients
}

// Request is a pending authorization request: the browser is at the
// identity provider or on the consent page.
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
	// UpsertClient inserts the client or replaces its name, redirect URIs
	// and fetch time.
	UpsertClient(ctx context.Context, c *Client) error
	// GetClient returns a client by id. ErrNotFound.
	GetClient(ctx context.Context, id string) (*Client, error)

	// CreateRequest stores a pending request.
	CreateRequest(ctx context.Context, r *Request) error
	// GetRequest returns a request, expired or not. ErrNotFound.
	GetRequest(ctx context.Context, id string) (*Request, error)
	// BindRequest binds a user to a request that has none yet. ErrNotFound
	// when the request is missing or already bound.
	BindRequest(ctx context.Context, id, handle string) error
	// DeleteRequest removes a request; a missing one is not an error.
	DeleteRequest(ctx context.Context, id string) error

	// CreateCode stores an authorization code.
	CreateCode(ctx context.Context, c *Code) error
	// ConsumeCode atomically marks the code used at at and returns it.
	// ErrNotFound for an unknown code, ErrCodeUsed for a second use.
	ConsumeCode(ctx context.Context, hash string, at time.Time) (*Code, error)

	// CreateTokens stores a new access and refresh pair that starts a new
	// family, setting both IDs and FamilyID (the refresh token's id).
	CreateTokens(ctx context.Context, access, refresh *Token) error
	// GetToken returns a token by hash, revoked or expired included.
	// ErrNotFound.
	GetToken(ctx context.Context, hash string) (*Token, error)
	// Rotate atomically revokes the live refresh token oldID at at, stores
	// access and refresh in its family, and records the new refresh token
	// as its replacement. ErrNotFound when oldID is missing, revoked or
	// already rotated.
	Rotate(ctx context.Context, oldID int64, at time.Time, access, refresh *Token) error
	// RevokeFamily revokes every live token of a family.
	RevokeFamily(ctx context.Context, familyID int64, at time.Time) error
	// ListGrants returns the user's live refresh tokens at now, newest
	// first.
	ListGrants(ctx context.Context, handle string, now time.Time) ([]Token, error)
	// RevokeGrant revokes the family of refresh token id. ErrNotFound.
	RevokeGrant(ctx context.Context, id int64, at time.Time) error
	// RevokeAllForUser revokes every live token of the user and deletes
	// their sessions.
	RevokeAllForUser(ctx context.Context, handle string, at time.Time) error
	// DeleteExpired removes requests, codes, tokens, sign-ins and sessions
	// expired at now and returns how many rows went.
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
	// DeleteSession removes a session with its codes and view sessions; a
	// missing one is not an error.
	DeleteSession(ctx context.Context, id int64) error

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
}

// IdentityProvider sends a browser to sign in and turns the callback into
// an Identity.
type IdentityProvider interface {
	// AuthURL is where the browser signs in; codeChallenge is the S256 PKCE
	// challenge.
	AuthURL(state, nonce, codeChallenge string) string
	// Exchange redeems the callback code and verifies the ID token.
	Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error)
}

// Keys checks API keys for grants approved without an identity provider:
// Authenticate verifies the key typed on the consent page, and KeyStatus
// reports later whether the key that approved a grant is still active.
// user.Service implements it.
type Keys interface {
	auth.Authenticator
	// KeyStatus returns the state of the key with prefix.
	// user.ErrKeyNotFound.
	KeyStatus(ctx context.Context, prefix string) (user.KeyStatus, error)
}

// Options configures a Service.
type Options struct {
	// Issuer is the authorization server, e.g. https://mcp.art.example.com.
	Issuer string
	// Resource is the MCP URL as clients enter it, e.g.
	// https://mcp.art.example.com/.
	Resource string
	// Lifetimes; zero means 1h, 30 days, 10m and 5m.
	AccessTTL, RefreshTTL, RequestTTL, CodeTTL time.Duration
	// Scopes offered to clients (default auth.MemberScopes). auth.ScopeAdmin
	// is dropped: a client an admin approves must not get a token that opens
	// the admin host.
	Scopes []string
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logger (default slog.Default()).
	Logger *slog.Logger
	// HTTPClient fetches client ID metadata documents. The default has a
	// 10 s timeout and refuses to connect to loopback, private and
	// link-local addresses.
	HTTPClient *http.Client
	// InsecureCookies drops the Secure cookie attribute and the __Host-
	// cookie name prefix, for development over plain http.
	InsecureCookies bool
	// ViewCallback returns the URL on an artifact host where a browser
	// sign-in ends (https://demo.art.example.com/.kazari/callback), given
	// the host's label, and whether the label names an artifact host. Nil
	// turns browser sign-in off.
	ViewCallback func(host string) (string, bool)
	// SessionTTL is how long a browser sign-in lasts (default 12h); every
	// view session it opens ends with it. ViewCodeTTL is the lifetime of
	// the code that carries it to an artifact host (default 1m).
	SessionTTL, ViewCodeTTL time.Duration
}

// Default lifetimes.
const (
	DefaultAccessTTL   = time.Hour
	DefaultRefreshTTL  = 30 * 24 * time.Hour
	DefaultRequestTTL  = 10 * time.Minute
	DefaultCodeTTL     = 5 * time.Minute
	DefaultSessionTTL  = 12 * time.Hour
	DefaultViewCodeTTL = time.Minute
)

// Service is the authorization server.
type Service struct {
	store  Store
	users  *user.Service
	keys   Keys
	idp    IdentityProvider
	audit  audit.Recorder
	opts   Options
	origin string // of the issuer, e.g. https://mcp.art.example.com
	now    func() time.Time
	log    *slog.Logger
	client *http.Client
	pages  *pages
}

var _ auth.Authenticator = (*Service)(nil)

// New returns a Service. keys checks the API key on the consent page when
// idp is nil; either may be nil (without keys, grants approved with a key
// stop working). rec may be nil.
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
		opts.HTTPClient = defaultHTTPClient()
	}
	var origin string
	if u, err := url.Parse(opts.Issuer); err == nil && u.Host != "" {
		origin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return &Service{
		store: store, users: users, keys: keys, idp: idp, audit: rec, opts: opts, origin: origin,
		now: opts.Now, log: opts.Logger, client: opts.HTTPClient, pages: newPages(),
	}
}

// defaultHTTPClient fetches metadata documents from public addresses only,
// so a client_id URL cannot make the server probe its own network.
func defaultHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: cimdTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !publicAddr(host) {
				return fmt.Errorf("oauth: refusing to fetch from non-public address %s", host)
			}
			return nil
		},
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	tr.Proxy = nil
	return &http.Client{Timeout: cimdTimeout, Transport: tr}
}

// nonPublic are special-purpose ranges a metadata fetch must never reach,
// beyond what the netip predicates cover: "this network", carrier-grade NAT
// (Tailscale and many self-hosted networks), IETF protocol assignments,
// documentation, benchmarking, reserved, and the IPv6 transition prefixes
// that can tunnel to an internal IPv4 address.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// publicAddr reports whether host, a literal IP from the dialer, is a
// public unicast address a client ID metadata document may live on.
func publicAddr(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Authenticate resolves an access token. Unknown, revoked or expired tokens,
// disabled users and grants whose approving API key is no longer active
// yield auth.ErrInvalidToken, so auth.Chain moves on. The principal's scopes
// are the token's capped at the offered scopes and at what the user's role
// allows now; it is never an admin. KeyID names the grant, so it stays the
// same across refreshes.
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
	_, live, err := s.approvingKey(ctx, t.KeyPrefix, t.UserHandle)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
	}
	if !live {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	// Options.Scopes never holds admin, so neither does a token issued
	// before that rule once capped here: an OAuth principal is never an
	// admin.
	return auth.Principal{
		Handle:     u.Handle,
		Scopes:     intersect(intersect(t.Scopes, s.opts.Scopes), user.ScopesFor(u)),
		Source:     auth.SourceOAuth,
		KeyID:      fmt.Sprintf("oauth:%d", t.FamilyID),
		QuotaBytes: u.QuotaBytes,
	}, nil
}

// approvingKey checks the API key that approved a grant; prefix "" (a
// grant approved after an identity-provider sign-in) always passes. live is
// false when the key is unknown, revoked or expired, belongs to someone
// other than handle, or its owner is disabled. expires is the key's expiry,
// nil when it has none.
func (s *Service) approvingKey(ctx context.Context, prefix, handle string) (expires *time.Time, live bool, err error) {
	if prefix == "" {
		return nil, true, nil
	}
	if s.keys == nil {
		return nil, false, nil
	}
	st, err := s.keys.KeyStatus(ctx, prefix)
	if errors.Is(err, user.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return st.ExpiresAt, st.Active && st.Handle == handle, nil
}

// GetClient returns a registered or cached client, e.g. to name the client
// of a grant. ErrNotFound.
func (s *Service) GetClient(ctx context.Context, id string) (*Client, error) {
	return s.store.GetClient(ctx, id)
}

// ListGrants returns the user's live refresh tokens, newest first.
func (s *Service) ListGrants(ctx context.Context, handle string) ([]Token, error) {
	return s.store.ListGrants(ctx, handle, s.now())
}

// RevokeGrant revokes refresh token id and every token of its family.
// ErrNotFound.
func (s *Service) RevokeGrant(ctx context.Context, id int64) error {
	if err := s.store.RevokeGrant(ctx, id, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, map[string]string{"grant": fmt.Sprint(id)})
	return nil
}

// RevokeAllForUser revokes every token of the user. It is wired to
// user.Options.OnDisable, so disabling a user ends their grants for good.
func (s *Service) RevokeAllForUser(ctx context.Context, handle string) error {
	if err := s.store.RevokeAllForUser(ctx, handle, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, map[string]string{"user": handle})
	return nil
}

// Cleanup deletes expired requests, codes and tokens and returns how many
// rows went. It is meant for a scheduler job.
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	return s.store.DeleteExpired(ctx, s.now())
}

func (s *Service) record(ctx context.Context, actor, action string, details map[string]string) {
	// Best-effort: a lost audit row must not fail the request.
	if err := s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Details: details}); err != nil {
		s.log.WarnContext(ctx, "oauth: audit", "action", action, "err", err)
	}
}

func actorFrom(ctx context.Context, fallback string) string {
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		return p.Handle
	}
	return fallback
}

// secretBytes is the entropy of codes, tokens and request ids.
const secretBytes = 32

// tokenLen is the length of an encoded secret.
var tokenLen = base64.RawURLEncoding.EncodedLen(secretBytes)

// randomString returns n random bytes, base64url without padding.
func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand panics on failure)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashSecret is the stored form of a code or token: hex SHA-256.
func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// s256 is the PKCE S256 challenge of a verifier.
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
