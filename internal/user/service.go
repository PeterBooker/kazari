package user

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agalma/internal/audit"
	"agalma/internal/auth"
)

// Handle length limits.
const (
	MinHandleLen = 2
	MaxHandleLen = 32
)

// touchInterval is how often last_used_at is written for one key.
const touchInterval = time.Minute

var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateHandle reports ErrInvalidHandle unless h is 2 to 32 characters of
// [a-z0-9_-] starting with a letter or digit.
func ValidateHandle(h string) error {
	if len(h) < MinHandleLen || len(h) > MaxHandleLen || !handleRE.MatchString(h) {
		return fmt.Errorf("%w: %q must be %d to %d characters of a-z, 0-9, _ and -, starting with a letter or digit",
			ErrInvalidHandle, h, MinHandleLen, MaxHandleLen)
	}
	return nil
}

// Options configures a Service. The zero value is usable: no default quota,
// no domain restriction for linking, no auto-provisioning.
type Options struct {
	// DefaultQuota is the quota of users created without one (0 = unlimited).
	DefaultQuota int64
	// AllowedDomains are the verified email domains that may sign in. Empty
	// means any verified email may link to an existing user, and nobody is
	// auto-provisioned.
	AllowedDomains []string
	// AutoProvision creates a member on first sign-in from an allowed domain.
	AutoProvision bool
	// AdminEmails are provisioned as admins.
	AdminEmails []string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Service manages users, API keys and identity links. It implements
// auth.Authenticator for API keys.
//
// Audit events are best-effort: they are recorded after the store call and
// a failure to record does not fail the operation.
type Service struct {
	store Store
	audit audit.Recorder
	opts  Options
	now   func() time.Time

	mu      sync.Mutex
	touched map[string]time.Time // key prefix -> last last_used_at write
}

var _ auth.Authenticator = (*Service)(nil)

// New returns a Service over store, recording to rec (nil discards).
func New(store Store, rec audit.Recorder, opts Options) *Service {
	if rec == nil {
		rec = audit.Nop{}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, audit: rec, opts: opts, now: now, touched: map[string]time.Time{}}
}

// CreateUserRequest describes a new user.
type CreateUserRequest struct {
	Handle      string
	Email       string
	DisplayName string
	Role        Role   // "" = RoleMember
	QuotaBytes  *int64 // nil = Options.DefaultQuota
}

// CreateUser validates req and creates the user. ErrInvalidHandle,
// ErrInvalidRole, ErrHandleTaken, ErrEmailTaken.
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	if err := ValidateHandle(req.Handle); err != nil {
		return nil, err
	}
	role := req.Role
	if role == "" {
		role = RoleMember
	}
	if err := validateRole(role); err != nil {
		return nil, err
	}
	quota := s.opts.DefaultQuota
	if req.QuotaBytes != nil {
		quota = *req.QuotaBytes
	}
	u := &User{
		Handle:      req.Handle,
		Email:       normalizeEmail(req.Email),
		DisplayName: strings.TrimSpace(req.DisplayName),
		Role:        role,
		QuotaBytes:  quota,
		CreatedAt:   s.now(),
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserCreate, map[string]string{
		"handle": u.Handle, "email": u.Email, "role": string(u.Role),
		"quota_bytes": strconv.FormatInt(u.QuotaBytes, 10),
	})
	return u, nil
}

func validateRole(r Role) error {
	if r != RoleAdmin && r != RoleMember {
		return fmt.Errorf("%w: %q", ErrInvalidRole, r)
	}
	return nil
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// GetUser returns the user with handle. ErrNotFound.
func (s *Service) GetUser(ctx context.Context, handle string) (*User, error) {
	return s.store.GetUser(ctx, handle)
}

// ListUsers returns every user ordered by handle.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	return s.store.ListUsers(ctx)
}

// UpdateUserRequest changes the non-nil fields of a user.
type UpdateUserRequest struct {
	Email       *string
	DisplayName *string
	Role        *Role
	QuotaBytes  *int64
	Disabled    *bool
}

// UpdateUser applies req to the user. Disabling a user stamps DisabledAt and
// revokes every key; enabling clears DisabledAt (revoked keys stay revoked).
func (s *Service) UpdateUser(ctx context.Context, handle string, req UpdateUserRequest) (*User, error) {
	u, err := s.store.GetUser(ctx, handle)
	if err != nil {
		return nil, err
	}
	details := map[string]string{"handle": handle}
	if req.Email != nil {
		u.Email = normalizeEmail(*req.Email)
		details["email"] = u.Email
	}
	if req.DisplayName != nil {
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
		details["display_name"] = u.DisplayName
	}
	if req.Role != nil {
		if err := validateRole(*req.Role); err != nil {
			return nil, err
		}
		u.Role = *req.Role
		details["role"] = string(u.Role)
	}
	if req.QuotaBytes != nil {
		u.QuotaBytes = *req.QuotaBytes
		details["quota_bytes"] = strconv.FormatInt(u.QuotaBytes, 10)
	}
	now := s.now()
	if req.Disabled != nil {
		if *req.Disabled {
			if u.DisabledAt == nil {
				u.DisabledAt = &now
			}
		} else {
			u.DisabledAt = nil
		}
		details["disabled"] = strconv.FormatBool(*req.Disabled)
	}
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	if req.Disabled != nil && *req.Disabled {
		if err := s.store.RevokeAllKeys(ctx, handle, now); err != nil {
			return nil, fmt.Errorf("user: disable %q: %w", handle, err)
		}
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, details)
	return u, nil
}

// ScopesFor returns the scopes the user's role allows: members get
// auth.MemberScopes, admins auth.AllScopes.
func ScopesFor(u *User) []string {
	if u.Role == RoleAdmin {
		return slices.Clone(auth.AllScopes)
	}
	return slices.Clone(auth.MemberScopes)
}

// IssueKey creates an API key for the user and returns it with the key
// string, which is never available again. Empty scopes mean
// auth.MemberScopes; expiresIn <= 0 means no expiry. ErrNotFound,
// ErrDisabled, ErrInvalidScope.
func (s *Service) IssueKey(ctx context.Context, handle, label string, scopes []string, expiresIn time.Duration) (*APIKey, string, error) {
	u, err := s.store.GetUser(ctx, handle)
	if err != nil {
		return nil, "", err
	}
	if u.DisabledAt != nil {
		return nil, "", fmt.Errorf("user: issue key for %q: %w", handle, ErrDisabled)
	}
	if len(scopes) == 0 {
		scopes = auth.MemberScopes
	}
	allowed := ScopesFor(u)
	var granted []string
	for _, sc := range auth.AllScopes { // canonical order, deduplicated
		if slices.Contains(scopes, sc) {
			granted = append(granted, sc)
		}
	}
	for _, sc := range scopes {
		if !slices.Contains(auth.AllScopes, sc) {
			return nil, "", fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, sc)
		}
		if !slices.Contains(allowed, sc) {
			return nil, "", fmt.Errorf("%w: role %s may not hold %q", ErrInvalidScope, u.Role, sc)
		}
	}

	key, prefix, err := newKey()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	k := &APIKey{
		UserHandle: handle,
		Prefix:     prefix,
		Hash:       HashKey(key),
		Label:      strings.TrimSpace(label),
		Scopes:     granted,
		CreatedAt:  now,
	}
	if expiresIn > 0 {
		exp := now.Add(expiresIn)
		k.ExpiresAt = &exp
	}
	if err := s.store.CreateKey(ctx, k); err != nil {
		return nil, "", err
	}
	s.record(ctx, actorFrom(ctx, handle), audit.KeyIssue, map[string]string{
		"handle": handle, "prefix": prefix, "label": k.Label, "scopes": strings.Join(granted, " "),
	})
	return k, key, nil
}

// ListKeys returns the user's keys, newest first.
func (s *Service) ListKeys(ctx context.Context, handle string) ([]APIKey, error) {
	return s.store.ListKeys(ctx, handle)
}

// RevokeKey revokes one of the user's keys. ErrKeyNotFound.
func (s *Service) RevokeKey(ctx context.Context, handle, prefix string) error {
	if err := s.store.RevokeKey(ctx, handle, prefix, s.now()); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, handle), audit.KeyRevoke, map[string]string{
		"handle": handle, "prefix": prefix,
	})
	return nil
}

// Authenticate verifies an API key. A malformed, unknown, revoked or expired
// key, a wrong secret, or a disabled owner yield auth.ErrInvalidToken; other
// store errors are returned wrapped. The principal's scopes are the key's
// scopes capped at ScopesFor the owner's current role.
func (s *Service) Authenticate(ctx context.Context, token string) (auth.Principal, error) {
	prefix, _, ok := ParseKey(token)
	if !ok {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	k, u, err := s.store.GetKey(ctx, prefix)
	if errors.Is(err, ErrKeyNotFound) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("user: authenticate: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(HashKey(token)), []byte(k.Hash)) != 1 {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	now := s.now()
	if k.RevokedAt != nil || (k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)) || u.DisabledAt != nil {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	s.touch(ctx, k, now)
	// Cap the key's scopes at what the role allows now, so an admin key of
	// a user later demoted to member loses the admin scope.
	allowed := ScopesFor(u)
	scopes := slices.DeleteFunc(slices.Clone(k.Scopes), func(sc string) bool {
		return !slices.Contains(allowed, sc)
	})
	return auth.Principal{
		Handle:     u.Handle,
		Admin:      u.Role == RoleAdmin,
		Scopes:     scopes,
		Source:     auth.SourceAPIKey,
		KeyID:      k.Prefix,
		QuotaBytes: u.QuotaBytes,
	}, nil
}

// touch writes last_used_at at most once per touchInterval per key. A
// failed write is retried on the next use and never fails authentication.
func (s *Service) touch(ctx context.Context, k *APIKey, now time.Time) {
	s.mu.Lock()
	last, seen := s.touched[k.Prefix]
	if !seen && k.LastUsedAt != nil {
		last, seen = *k.LastUsedAt, true
	}
	if seen && now.Sub(last) < touchInterval {
		s.touched[k.Prefix] = last
		s.mu.Unlock()
		return
	}
	s.touched[k.Prefix] = now
	s.mu.Unlock()

	if err := s.store.TouchKey(ctx, k.Prefix, now); err != nil {
		s.mu.Lock()
		if s.touched[k.Prefix].Equal(now) {
			delete(s.touched, k.Prefix)
		}
		s.mu.Unlock()
	}
}

// ResolveIdentity maps an upstream identity to a user:
//
//  1. an identity already linked by (issuer, subject) resolves to its user;
//  2. otherwise a verified email from an allowed domain (any domain when
//     AllowedDomains is empty) links to the user with that email;
//  3. otherwise, with AutoProvision on and a verified email from an allowed
//     domain, a new user is created (an admin when the email is in
//     AdminEmails) with a handle derived from the email;
//  4. otherwise ErrNotProvisioned.
//
// A disabled user yields ErrDisabled. On success the identity link is
// refreshed so last_login_at moves.
func (s *Service) ResolveIdentity(ctx context.Context, id auth.Identity) (*User, error) {
	u, err := s.store.GetIdentity(ctx, id.Issuer, id.Subject)
	switch {
	case err == nil:
		return s.login(ctx, u, id)
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	email := normalizeEmail(id.Email)
	if email == "" || !id.EmailVerified {
		return nil, ErrNotProvisioned
	}
	_, domain, _ := strings.Cut(email, "@")
	if domain == "" {
		return nil, ErrNotProvisioned
	}
	domainListed := slices.ContainsFunc(s.opts.AllowedDomains, func(d string) bool {
		return strings.EqualFold(strings.TrimSpace(d), domain)
	})

	if len(s.opts.AllowedDomains) == 0 || domainListed {
		u, err := s.store.GetUserByEmail(ctx, email)
		switch {
		case err == nil:
			return s.login(ctx, u, id)
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}

	if !s.opts.AutoProvision || !domainListed {
		return nil, ErrNotProvisioned
	}
	role := RoleMember
	if slices.ContainsFunc(s.opts.AdminEmails, func(e string) bool { return normalizeEmail(e) == email }) {
		role = RoleAdmin
	}
	u, err = s.provision(ctx, email, strings.TrimSpace(id.Name), role)
	if err != nil {
		return nil, err
	}
	return s.login(ctx, u, id)
}

// login refuses a disabled user and refreshes the identity link.
func (s *Service) login(ctx context.Context, u *User, id auth.Identity) (*User, error) {
	if u.DisabledAt != nil {
		return nil, fmt.Errorf("user: sign in as %q: %w", u.Handle, ErrDisabled)
	}
	err := s.store.LinkIdentity(ctx, u.Handle, Identity{
		Issuer:       id.Issuer,
		Subject:      id.Subject,
		EmailAtLogin: id.Email,
		LastLoginAt:  s.now(),
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ErrInvalidIdentity is returned by LinkIdentity and UnlinkIdentity when the
// issuer or subject is empty.
var ErrInvalidIdentity = errors.New("user: identity needs an issuer and a subject")

// ListIdentities returns the user's linked identities ordered by issuer and
// subject. ErrNotFound when the user does not exist.
func (s *Service) ListIdentities(ctx context.Context, handle string) ([]Identity, error) {
	return s.store.ListIdentities(ctx, handle)
}

// LinkIdentity links an upstream (issuer, subject) to the user by hand, so
// an admin can pre-provision who may sign in as whom. An identity linked to
// another user moves to this one. ErrInvalidIdentity, ErrNotFound.
func (s *Service) LinkIdentity(ctx context.Context, handle, issuer, subject, email string) error {
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ErrInvalidIdentity
	}
	err := s.store.LinkIdentity(ctx, handle, Identity{
		Issuer:       issuer,
		Subject:      subject,
		EmailAtLogin: normalizeEmail(email),
		LastLoginAt:  s.now(),
	})
	if err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, map[string]string{
		"handle": handle, "identity": "link", "issuer": issuer, "subject": subject,
	})
	return nil
}

// UnlinkIdentity removes a linked identity from the user. ErrInvalidIdentity,
// or ErrNotFound when the user has no such identity.
func (s *Service) UnlinkIdentity(ctx context.Context, handle, issuer, subject string) error {
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ErrInvalidIdentity
	}
	if err := s.store.UnlinkIdentity(ctx, handle, issuer, subject); err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, map[string]string{
		"handle": handle, "identity": "unlink", "issuer": issuer, "subject": subject,
	})
	return nil
}

// maxHandleAttempts bounds the numeric suffix search.
const maxHandleAttempts = 1000

// provision creates a user for email, suffixing the derived handle until it
// is free.
func (s *Service) provision(ctx context.Context, email, name string, role Role) (*User, error) {
	base := DeriveHandle(email)
	for n := 1; n <= maxHandleAttempts; n++ {
		u := &User{
			Handle:      handleWithSuffix(base, n),
			Email:       email,
			DisplayName: name,
			Role:        role,
			QuotaBytes:  s.opts.DefaultQuota,
			CreatedAt:   s.now(),
		}
		err := s.store.CreateUser(ctx, u)
		if errors.Is(err, ErrHandleTaken) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s.record(ctx, u.Handle, audit.UserCreate, map[string]string{
			"handle": u.Handle, "email": u.Email, "role": string(u.Role),
			"quota_bytes": strconv.FormatInt(u.QuotaBytes, 10), "via": "identity",
		})
		return u, nil
	}
	return nil, fmt.Errorf("user: provision %s: no free handle after %d attempts: %w",
		email, maxHandleAttempts, ErrHandleTaken)
}

// DeriveHandle turns an email's local part into a valid handle: lower-cased,
// characters outside [a-z0-9_-] replaced by "-", leading "-" and "_" and
// trailing "-" trimmed, truncated to MaxHandleLen. A result shorter than
// MinHandleLen becomes "user".
func DeriveHandle(email string) string {
	local := strings.ToLower(strings.TrimSpace(email))
	if i := strings.LastIndex(local, "@"); i >= 0 {
		local = local[:i]
	}
	h := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, local)
	h = strings.TrimLeft(h, "-_")
	if len(h) > MaxHandleLen {
		h = h[:MaxHandleLen]
	}
	h = strings.TrimRight(h, "-")
	if len(h) < MinHandleLen {
		return "user"
	}
	return h
}

// handleWithSuffix returns base for n == 1 and base-n otherwise, shortening
// base so the result fits MaxHandleLen.
func handleWithSuffix(base string, n int) string {
	if n == 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	if len(base)+len(suffix) > MaxHandleLen {
		base = strings.TrimRight(base[:MaxHandleLen-len(suffix)], "-")
	}
	return base + suffix
}

// actorFrom names the caller for audit: the principal in ctx, else fallback.
func actorFrom(ctx context.Context, fallback string) string {
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		return p.Handle
	}
	return fallback
}

func (s *Service) record(ctx context.Context, actor, action string, details map[string]string) {
	// Best-effort by design: see Service.
	_ = s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Details: details})
}
