// Package oauth is the OAuth 2.1 authorization server for MCP clients: RFC
// 8414 and RFC 9728 metadata, dynamic registration (RFC 7591), client ID
// metadata documents, PKCE (S256 only) and refresh-token rotation. People sign
// in at an upstream IdentityProvider or, without one, with an API key whose
// lifetime then bounds the grant; the admin scope is never granted.
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

const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

const (
	ActionGrant  = "oauth.grant"
	ActionRevoke = "oauth.revoke"
)

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

// Store is the persistence contract. Users are referenced by handle.
type Store interface {
	// UpsertClient inserts c or replaces its name, redirect URIs and fetch time.
	UpsertClient(ctx context.Context, c *Client) error
	// GetClient returns ErrNotFound for an unknown id.
	GetClient(ctx context.Context, id string) (*Client, error)

	CreateRequest(ctx context.Context, r *Request) error
	// GetRequest returns a request, expired or not, or ErrNotFound.
	GetRequest(ctx context.Context, id string) (*Request, error)
	// BindRequest returns ErrNotFound when the request is missing or bound.
	BindRequest(ctx context.Context, id, handle string) error
	// DeleteRequest of a missing request is not an error.
	DeleteRequest(ctx context.Context, id string) error

	CreateCode(ctx context.Context, c *Code) error
	// ConsumeCode atomically marks the code used at at and returns it.
	// ErrNotFound for an unknown code, ErrCodeUsed for a second use.
	ConsumeCode(ctx context.Context, hash string, at time.Time) (*Code, error)

	// CreateTokens stores a pair that starts a new family, setting both IDs
	// and FamilyID (the refresh token's id).
	CreateTokens(ctx context.Context, access, refresh *Token) error
	// GetToken returns revoked and expired tokens too, or ErrNotFound.
	GetToken(ctx context.Context, hash string) (*Token, error)
	// Rotate atomically revokes the live refresh token oldID, stores access
	// and refresh in its family and records refresh as its replacement.
	// ErrNotFound when oldID is missing, revoked or already rotated.
	Rotate(ctx context.Context, oldID int64, at time.Time, access, refresh *Token) error
	RevokeFamily(ctx context.Context, familyID int64, at time.Time) error
	// ListGrants returns the user's live refresh tokens, newest first.
	ListGrants(ctx context.Context, handle string, now time.Time) ([]Token, error)
	// RevokeGrant revokes the family of refresh token id. ErrNotFound.
	RevokeGrant(ctx context.Context, id int64, at time.Time) error
	RevokeAllForUser(ctx context.Context, handle string, at time.Time) error
	// DeleteExpired returns the number of requests, codes and tokens removed.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}

// IdentityProvider sends a browser to sign in and turns the callback into
// an Identity.
type IdentityProvider interface {
	// AuthURL takes the S256 PKCE challenge.
	AuthURL(state, nonce, codeChallenge string) string
	// Exchange redeems the callback code and verifies the ID token.
	Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error)
}

// Keys checks the API keys of grants approved without an identity provider,
// at consent and on every later use. user.Service implements it.
type Keys interface {
	auth.Authenticator
	// KeyStatus returns user.ErrKeyNotFound for an unknown prefix.
	KeyStatus(ctx context.Context, prefix string) (user.KeyStatus, error)
}

// Options configures a Service.
type Options struct {
	Issuer string // e.g. https://mcp.art.example.com
	// Resource is the MCP URL as clients enter it, e.g.
	// https://mcp.art.example.com/.
	Resource string
	// Zero means the Default*TTL.
	AccessTTL, RefreshTTL, RequestTTL, CodeTTL time.Duration
	// Scopes offered to clients (default auth.MemberScopes). auth.ScopeAdmin
	// is dropped so a client an admin approves cannot open the admin host.
	Scopes []string
	Now    func() time.Time
	Logger *slog.Logger
	// HTTPClient fetches client ID metadata documents. The default times out
	// after 10 s and dials public addresses only.
	HTTPClient *http.Client
	// InsecureCookies drops the Secure attribute and the __Host- prefix, for
	// development over plain http.
	InsecureCookies bool
}

const (
	DefaultAccessTTL  = time.Hour
	DefaultRefreshTTL = 30 * 24 * time.Hour
	DefaultRequestTTL = 10 * time.Minute
	DefaultCodeTTL    = 5 * time.Minute
)

// Service is the authorization server.
type Service struct {
	store  Store
	users  *user.Service
	keys   Keys
	idp    IdentityProvider
	audit  audit.Recorder
	opts   Options
	origin string // of the issuer
	now    func() time.Time
	log    *slog.Logger
	client *http.Client
	pages  *pages
}

var _ auth.Authenticator = (*Service)(nil)

// New returns a Service. keys checks the consent page's API key when idp is
// nil; keys, idp and rec may be nil, but without keys grants approved with a
// key stop working.
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

// defaultHTTPClient dials public addresses only, so a client_id URL cannot
// make the server probe its own network (SSRF).
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

// nonPublic are special-purpose ranges the netip predicates miss, among them
// carrier-grade NAT (Tailscale) and IPv6 transition prefixes that can tunnel
// to an internal IPv4 address.
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

// publicAddr reports whether host, a literal IP, is public unicast.
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
	_, live, err := s.approvingKey(ctx, t.KeyPrefix, t.UserHandle)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
	}
	if !live {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	// Capping at Options.Scopes also strips admin from tokens issued
	// before admin was dropped.
	return auth.Principal{
		Handle:     u.Handle,
		Scopes:     intersect(intersect(t.Scopes, s.opts.Scopes), user.ScopesFor(u)),
		Source:     auth.SourceOAuth,
		KeyID:      fmt.Sprintf("oauth:%d", t.FamilyID),
		QuotaBytes: u.QuotaBytes,
	}, nil
}

// approvingKey passes prefix "" (an identity-provider sign-in). live is false
// when the key is unknown, inactive or not handle's; expires is nil when it
// never expires.
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

// GetClient returns a registered or cached client, or ErrNotFound.
func (s *Service) GetClient(ctx context.Context, id string) (*Client, error) {
	return s.store.GetClient(ctx, id)
}

// ListGrants returns the user's live refresh tokens, newest first.
func (s *Service) ListGrants(ctx context.Context, handle string) ([]Token, error) {
	return s.store.ListGrants(ctx, handle, s.now())
}

// RevokeGrant revokes the family of refresh token id. ErrNotFound.
func (s *Service) RevokeGrant(ctx context.Context, id int64) error {
	if err := s.store.RevokeGrant(ctx, id, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, map[string]string{"grant": fmt.Sprint(id)})
	return nil
}

// RevokeAllForUser revokes every token of the user. It is wired to
// user.Options.OnDisable, so re-enabling a user does not restore them.
func (s *Service) RevokeAllForUser(ctx context.Context, handle string) error {
	if err := s.store.RevokeAllForUser(ctx, handle, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), ActionRevoke, map[string]string{"user": handle})
	return nil
}

// Cleanup deletes expired requests, codes and tokens, returning the count.
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	return s.store.DeleteExpired(ctx, s.now())
}

func (s *Service) record(ctx context.Context, actor, action string, details map[string]string) {
	// A lost audit row must not fail the request.
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
