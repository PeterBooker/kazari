// Package oauth is the OAuth 2.1 authorization server for MCP clients: RFC
// 8414 and RFC 9728 metadata, dynamic registration (RFC 7591), client ID
// metadata documents, PKCE (S256 only) and refresh-token rotation. People sign
// in at an upstream IdentityProvider or, without one, with the password of a
// built-in account or an API key, through the browser sign-in host that
// shares the MCP host (SignIn); a grant approved with a key takes that key's
// scopes, restriction and lifetime. The admin scope is never granted.
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
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/cookie"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

const (
	ActionGrant  = "oauth.grant"
	ActionRevoke = "oauth.revoke"
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
	// RevokeAllForUser also deletes the user's browser sessions.
	RevokeAllForUser(ctx context.Context, handle string, at time.Time) error
	// DeleteExpired returns the number of requests, codes and tokens
	// removed.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}

// IdentityProvider is where people sign in for an MCP client when the
// instance has one. signin.IdentityProvider is one.
type IdentityProvider interface {
	// AuthURL takes the S256 PKCE challenge.
	AuthURL(state, nonce, codeChallenge string) string
}

// Users are the people grants are made to. user.Service implements it.
type Users interface {
	// ActiveUser returns user.ErrNotFound for a person who is unknown or
	// disabled.
	ActiveUser(ctx context.Context, handle string) (*user.User, error)
}

// Keys checks the API keys that grants are approved with, on every use of
// the grant. user.Service implements it.
type Keys interface {
	// KeyStatus returns user.ErrKeyNotFound for an unknown prefix.
	KeyStatus(ctx context.Context, prefix string) (user.KeyStatus, error)
}

// SignIn is the browser sign-in host, which shares the MCP host and signs
// people in for an MCP client's authorization. signin.Service implements
// it.
type SignIn interface {
	// Mount adds the sign-in host's pages to the MCP host's mux; a sign-in
	// for an authorization goes on through requests.
	Mount(mux *http.ServeMux, requests signin.Requests)
	// Passwords reports whether people sign in with the password of a
	// built-in account.
	Passwords() bool
	// Session returns the browser's live session, nil when it has none.
	Session(r *http.Request) (*signin.Session, error)
	// ShowRequestForm writes the sign-in form for an authorization.
	ShowRequestForm(w http.ResponseWriter, r *http.Request, f signin.RequestForm)
	// Unavailable reports, and answers, a sign-in the identity provider
	// cannot take yet.
	Unavailable(w http.ResponseWriter, r *http.Request) bool
	// ServeCallback finishes a sign-in for a host at the identity
	// provider, whose state starts with signin.StatePrefix.
	ServeCallback(w http.ResponseWriter, r *http.Request)
	// Identify redeems the identity provider's callback and returns the
	// person it signed in. When ok is false it has answered, with again as
	// its advice; refused runs first when the person is not admitted.
	Identify(w http.ResponseWriter, r *http.Request, nonce, verifier, again string, refused func()) (*user.User, bool)
	// CheckKey authenticates an API key typed into a form, counted as a
	// sign-in with a key.
	CheckKey(ctx context.Context, key string) (auth.Principal, error)
}

// Deps are the collaborators of the authorization server. Users is
// required, and SignIn for Handler. Keys, IdP and Audit may be nil, but
// without Keys grants approved with a key stop working.
type Deps struct {
	Users  Users
	Keys   Keys
	IdP    IdentityProvider
	SignIn SignIn
	Audit  audit.Recorder
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
}

const (
	DefaultAccessTTL  = time.Hour
	DefaultRefreshTTL = 90 * 24 * time.Hour
	DefaultRequestTTL = 10 * time.Minute
	DefaultCodeTTL    = 5 * time.Minute
	DefaultClientTTL  = 24 * time.Hour
)

// DefaultMaxUnusedClients with maxRedirectURIs of maxRedirectURILen bounds
// what anonymous callers can keep in the database to about 400 MiB of redirect
// URIs, or 2.4 GiB at worst, because their stored JSON escapes & < > to six
// bytes.
const DefaultMaxUnusedClients = 10_000

// Service is the authorization server.
type Service struct {
	store   Store
	users   Users
	keys    Keys
	idp     IdentityProvider
	signIn  SignIn
	audit   audit.Recorder
	opts    Options
	origin  string // of the issuer
	now     func() time.Time
	log     *slog.Logger
	client  *http.Client
	cookies cookie.Host
	pages   *signin.Pages
	tracer  trace.Tracer
	m       metrics
}

var _ auth.Authenticator = (*Service)(nil)

// New returns a Service.
func New(store Store, d Deps, opts Options) *Service {
	if d.Audit == nil {
		d.Audit = audit.Nop{}
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
	return &Service{
		store: store, users: d.Users, keys: d.Keys, idp: d.IdP, signIn: d.SignIn, audit: d.Audit, opts: opts, origin: origin,
		now: opts.Now, log: opts.Logger, client: opts.HTTPClient, cookies: cookie.Host{Insecure: opts.InsecureCookies},
		pages:  signin.NewPages(origin, templateFS, "templates/consent.html"),
		tracer: opts.Telemetry.Tracer(scope), m: newMetrics(opts.Telemetry.Meter(scope)),
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
	u, err := s.users.ActiveUser(ctx, t.UserHandle)
	if errors.Is(err, user.ErrNotFound) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("oauth: authenticate: %w", err)
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
		Role:       string(u.Role),
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

// approvingKey reads the key a grant was approved with. Prefix ""
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

// Cleanup deletes expired requests, codes and tokens, and clients
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
