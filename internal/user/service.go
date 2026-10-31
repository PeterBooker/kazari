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

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
)

const (
	MinHandleLen = 2
	MaxHandleLen = 32
)

const touchInterval = time.Minute

var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ReservedHandles name the actors Kazari itself writes to the audit log,
// the admin token and socket and the scheduler, so no user can be mistaken
// for them.
var ReservedHandles = []string{"admin", "scheduler"}

// ValidateHandle reports ErrInvalidHandle unless h is 2 to 32 characters of
// [a-z0-9_-] starting with a letter or digit, and not reserved.
func ValidateHandle(h string) error {
	if len(h) < MinHandleLen || len(h) > MaxHandleLen || !handleRE.MatchString(h) {
		return fmt.Errorf("%w: %q must be %d to %d characters of a-z, 0-9, _ and -, starting with a letter or digit",
			ErrInvalidHandle, h, MinHandleLen, MaxHandleLen)
	}
	if slices.Contains(ReservedHandles, h) {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidHandle, h)
	}
	return nil
}

// Options configures a Service. The zero value is usable.
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
	// OnDisable, if set, runs after a user is disabled and their keys revoked,
	// to revoke credentials held elsewhere (OAuth grants). An error fails the
	// update; the user stays disabled.
	OnDisable func(ctx context.Context, handle string) error
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service manages users, API keys and identity links. Audit events are
// best-effort: a failure to record one does not fail the operation.
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

// CreateUser creates a user. ErrInvalidHandle, ErrInvalidRole,
// ErrHandleTaken, ErrEmailTaken.
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

// QuotaBytes returns the user's quota, 0 meaning unlimited, for versions
// that other members add to their artifacts. ErrNotFound.
func (s *Service) QuotaBytes(ctx context.Context, handle string) (int64, error) {
	u, err := s.store.GetUser(ctx, handle)
	if err != nil {
		return 0, err
	}
	return u.QuotaBytes, nil
}

// UpdateUserRequest changes the non-nil fields of a user.
type UpdateUserRequest struct {
	Email       *string
	DisplayName *string
	Role        *Role
	QuotaBytes  *int64
	Disabled    *bool
}

// UpdateUser applies req. Disabling stamps DisabledAt, revokes every key and
// runs Options.OnDisable; enabling clears DisabledAt (revoked keys and grants
// stay revoked).
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
		if s.opts.OnDisable != nil {
			if err := s.opts.OnDisable(ctx, handle); err != nil {
				return nil, fmt.Errorf("user: disable %q: %w", handle, err)
			}
		}
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, details)
	return u, nil
}

// ScopesFor returns the scopes u's role allows.
func ScopesFor(u *User) []string {
	if u.Role == RoleAdmin {
		return slices.Clone(auth.AllScopes)
	}
	return slices.Clone(auth.MemberScopes)
}

// IssueKey creates an API key and returns it with the key string, which is
// never available again. Empty scopes mean auth.MemberScopes; expiresIn <= 0
// means no expiry. ErrNotFound, ErrDisabled, ErrInvalidScope.
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
// key, a wrong secret or a disabled owner yield auth.ErrInvalidToken; other
// store errors are wrapped. Scopes are capped at the owner's current role, so
// a demoted admin's key loses the admin scope.
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
	if !keyActive(k, u, now) {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	s.touch(ctx, k, now)
	allowed := ScopesFor(u)
	scopes := slices.DeleteFunc(slices.Clone(k.Scopes), func(sc string) bool {
		return !slices.Contains(allowed, sc)
	})
	// An admin's key issued with member scopes acts as a member, so a
	// narrowed key stays narrow.
	return auth.Principal{
		Handle:     u.Handle,
		Admin:      u.Role == RoleAdmin && slices.Contains(scopes, auth.ScopeAdmin),
		Scopes:     scopes,
		Source:     auth.SourceAPIKey,
		KeyID:      k.Prefix,
		QuotaBytes: u.QuotaBytes,
	}, nil
}

// Recognize implements auth.Recognizer for API keys.
func (s *Service) Recognize(token string) (string, bool) {
	_, _, ok := ParseKey(token)
	return auth.SourceAPIKey, ok
}

// KeyStatus reports on the key with prefix, for credentials derived from it
// (an OAuth grant lives only while its key is active). ErrKeyNotFound.
func (s *Service) KeyStatus(ctx context.Context, prefix string) (KeyStatus, error) {
	k, u, err := s.store.GetKey(ctx, prefix)
	if err != nil {
		return KeyStatus{}, err
	}
	return KeyStatus{Handle: u.Handle, Active: keyActive(k, u, s.now()), ExpiresAt: k.ExpiresAt}, nil
}

func keyActive(k *APIKey, u *User, now time.Time) bool {
	return k.RevokedAt == nil && (k.ExpiresAt == nil || now.Before(*k.ExpiresAt)) && u.DisabledAt == nil
}

// touch writes last_used_at at most once per touchInterval per key. A failed
// write is retried on the next use and never fails authentication.
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

// ResolveIdentity maps an upstream identity to, in order: the user linked to
// (issuer, subject); the user with its verified email, if the domain is
// allowed (any domain when AllowedDomains is empty) and, for an admin, the
// email is in AdminEmails; a new user, an admin if in AdminEmails, when
// AutoProvision is on and the domain is listed; else ErrNotProvisioned. A
// disabled user yields ErrDisabled. Success refreshes the link's
// last_login_at.
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
			// A verified address alone must not take over an admin
			// account: the operator either lists the address or links the
			// identity by hand.
			if u.Role == RoleAdmin && u.DisabledAt == nil && !s.adminEmail(email) {
				return nil, fmt.Errorf("%w: admin %q links by email only when admin_emails names it; link the identity instead",
					ErrNotProvisioned, u.Handle)
			}
			return s.login(ctx, u, id)
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}

	if !s.opts.AutoProvision || !domainListed {
		return nil, ErrNotProvisioned
	}
	role := RoleMember
	if s.adminEmail(email) {
		role = RoleAdmin
	}
	u, err = s.provision(ctx, email, strings.TrimSpace(id.Name), role)
	if err != nil {
		return nil, err
	}
	return s.login(ctx, u, id)
}

// adminEmail reports whether Options.AdminEmails names the normalised email.
func (s *Service) adminEmail(email string) bool {
	return slices.ContainsFunc(s.opts.AdminEmails, func(e string) bool { return normalizeEmail(e) == email })
}

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

var ErrInvalidIdentity = errors.New("user: identity needs an issuer and a subject")

// ListIdentities returns the user's identities ordered by issuer and
// subject. ErrNotFound if the user does not exist.
func (s *Service) ListIdentities(ctx context.Context, handle string) ([]Identity, error) {
	return s.store.ListIdentities(ctx, handle)
}

// LinkIdentity links (issuer, subject) to the user by hand, so an admin can
// pre-provision who may sign in as whom. An identity linked to another user
// moves to this one. ErrInvalidIdentity, ErrNotFound.
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

// UnlinkIdentity returns ErrInvalidIdentity, or ErrNotFound when the user has
// no such identity.
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

const maxHandleAttempts = 1000

func (s *Service) provision(ctx context.Context, email, name string, role Role) (*User, error) {
	base := DeriveHandle(email)
	for n := 1; n <= maxHandleAttempts; n++ {
		if slices.Contains(ReservedHandles, handleWithSuffix(base, n)) {
			continue
		}
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

// DeriveHandle turns an email's local part into a valid handle, or "user"
// when too little of it survives.
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

func actorFrom(ctx context.Context, fallback string) string {
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		return p.Handle
	}
	return fallback
}

func (s *Service) record(ctx context.Context, actor, action string, details map[string]string) {
	// Best-effort: see Service.
	_ = s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Details: details})
}
