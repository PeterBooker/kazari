package user

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/PeterBooker/kazari/internal/instrument"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
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

// SuggestHandle returns a handle made from s, such as ana-admin from
// "Ana Admin", or "" when nothing usable is left. A valid handle is its own
// suggestion.
func SuggestHandle(s string) string {
	if ValidateHandle(s) == nil {
		return s
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= MaxHandleLen {
			break
		}
	}
	h := strings.TrimRight(strings.TrimLeft(b.String(), "_"), "-")
	if ValidateHandle(h) != nil {
		return ""
	}
	return h
}

// Options configures a Service. The zero value is usable.
type Options struct {
	// DefaultQuota is the quota of users created without one (0 = unlimited).
	DefaultQuota int64
	// AllowedDomains are the verified email domains that may sign in. Empty
	// means any verified email may link to an existing user, and nobody
	// outside AdminEmails is auto-provisioned.
	AllowedDomains []string
	// AutoProvision creates a member on first sign-in from an allowed domain.
	AutoProvision bool
	// AdminEmails are provisioned as admins on first sign-in, whatever
	// AllowedDomains and AutoProvision say: the operator named each.
	AdminEmails []string
	// MaxKeyTTL, when set, is the longest a key may live: one issued without
	// an expiry gets it, and a longer one is refused. Keys issued before it
	// was set keep what they have.
	MaxKeyTTL time.Duration
	// MaxKeys is how many live keys a person may hold, however they are
	// issued (default DefaultMaxKeys).
	MaxKeys int
	// OnDisable, if set, runs after a user is disabled and their keys revoked,
	// to revoke credentials held elsewhere (OAuth grants, share links). An
	// error fails the update; the user stays disabled.
	OnDisable func(ctx context.Context, handle string) error

	// LocalAccounts turns the built-in accounts on: passwords and the links
	// that set them. Off, their methods answer ErrLocalOff and no password
	// signs anyone in.
	LocalAccounts bool
	// MinPasswordLength is the shortest password accepted (default
	// DefaultMinPasswordLength).
	MinPasswordLength int
	// How long an invite, a reset and the setup link work (default
	// DefaultInviteTTL, DefaultResetTTL, DefaultSetupTTL).
	InviteTTL, ResetTTL, SetupTTL time.Duration
	// Argon2 are the parameters of new password hashes (default
	// DefaultArgon2); MaxHashes how many are computed at once (default
	// DefaultMaxHashes).
	Argon2    Argon2
	MaxHashes int
	// EndSessions, if set, ends the user's browser sessions except the one
	// with id keep (0 keeps none) after their password changed. RevokeGrants,
	// if set, revokes their OAuth grants and every session.
	EndSessions  func(ctx context.Context, handle string, keep int64) error
	RevokeGrants func(ctx context.Context, handle string) error
	// Now defaults to time.Now.
	Now       func() time.Time
	Telemetry instrument.Providers
}

// Service manages users, API keys and identity links. Audit events are
// best-effort: a failure to record one does not fail the operation.
type Service struct {
	store   Store
	audit   audit.Recorder
	opts    Options
	now     func() time.Time
	tracer  trace.Tracer
	m       metrics
	hashing hashing

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
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = DefaultMaxKeys
	}
	if opts.MinPasswordLength <= 0 {
		opts.MinPasswordLength = DefaultMinPasswordLength
	}
	if opts.InviteTTL <= 0 {
		opts.InviteTTL = DefaultInviteTTL
	}
	if opts.ResetTTL <= 0 {
		opts.ResetTTL = DefaultResetTTL
	}
	if opts.SetupTTL <= 0 {
		opts.SetupTTL = DefaultSetupTTL
	}
	if opts.Argon2 == (Argon2{}) {
		opts.Argon2 = DefaultArgon2
	}
	if opts.MaxHashes <= 0 {
		opts.MaxHashes = DefaultMaxHashes
	}
	s := &Service{store: store, audit: rec, opts: opts, now: now, tracer: opts.Telemetry.Tracer(scope),
		m: newMetrics(opts.Telemetry.Meter(scope)), touched: map[string]time.Time{}}
	s.hashing = hashing{
		took: func(ctx context.Context, operation string, d time.Duration) {
			s.m.hashes.Record(ctx, d.Seconds(), metric.WithAttributes(hashOperationKey.String(operation)))
		},
		params: opts.Argon2,
		slots:  make(chan struct{}, opts.MaxHashes),
		wait:   slotWait,
		// Made on first use: a hash costs too much for every start to pay.
		dummy: sync.OnceValue(func() string {
			h, err := hashPassword(opts.Argon2, "")
			if err != nil {
				panic(err) // crypto/rand does not fail
			}
			return h
		}),
	}
	s.observeUsers(opts.Telemetry.Meter(scope))
	return s
}

// DefaultMaxKeys is how many live keys a person may hold.
const DefaultMaxKeys = 50

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
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.create")
	defer func() { endSpan(span, err) }()
	u, err := s.newUser(req)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	s.recordCreated(ctx, u)
	return u, nil
}

// newUser validates req and returns the user it describes, not yet stored.
func (s *Service) newUser(req CreateUserRequest) (*User, error) {
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
	return &User{
		Handle:      req.Handle,
		Email:       normalizeEmail(req.Email),
		DisplayName: strings.TrimSpace(req.DisplayName),
		Role:        role,
		QuotaBytes:  quota,
		CreatedAt:   s.now(),
	}, nil
}

func (s *Service) recordCreated(ctx context.Context, u *User) {
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserCreate, u.Handle, map[string]string{
		"handle": u.Handle, "email": u.Email, "role": string(u.Role),
		"quota_bytes": strconv.FormatInt(u.QuotaBytes, 10),
	})
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
// runs Options.OnDisable; enabling clears DisabledAt (revoked keys, grants and
// share links stay revoked). ErrSelf when the caller disables or demotes
// their own account, ErrLastAdmin when the change would leave no active
// admin: the admin socket and the admin token, which are no person, then
// stay the way back in.
func (s *Service) UpdateUser(ctx context.Context, handle string, req UpdateUserRequest) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.update")
	defer func() { endSpan(span, err) }()
	if req.Role != nil {
		if err := validateRole(*req.Role); err != nil {
			return nil, err
		}
	}
	caller, _ := auth.FromContext(ctx)
	now := s.now()
	details := map[string]string{"handle": handle}
	// The change is made on the row as it is inside the store's
	// transaction: one built from an earlier read would write back what
	// another admin has changed since, such as a disable.
	u, err := s.store.UpdateUser(ctx, handle, func(u *User) error {
		disables := req.Disabled != nil && *req.Disabled
		demotes := req.Role != nil && *req.Role != RoleAdmin && u.Role == RoleAdmin
		if caller.Handle == handle && (disables || demotes) {
			return ErrSelf
		}
		if req.Email != nil {
			u.Email = normalizeEmail(*req.Email)
			details["email"] = u.Email
		}
		if req.DisplayName != nil {
			u.DisplayName = strings.TrimSpace(*req.DisplayName)
			details["display_name"] = u.DisplayName
		}
		if req.Role != nil {
			u.Role = *req.Role
			details["role"] = string(u.Role)
		}
		if req.QuotaBytes != nil {
			u.QuotaBytes = *req.QuotaBytes
			details["quota_bytes"] = strconv.FormatInt(u.QuotaBytes, 10)
		}
		if req.Disabled != nil {
			if !*req.Disabled {
				u.DisabledAt = nil
			} else if u.DisabledAt == nil {
				u.DisabledAt = &now
			}
			details["disabled"] = strconv.FormatBool(*req.Disabled)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if req.Disabled != nil && *req.Disabled {
		if err := s.store.RevokeAllKeys(ctx, handle, now); err != nil {
			return nil, fmt.Errorf("user: disable %q: %w", handle, err)
		}
		// Their links too: one left live would work again the moment the
		// person is enabled.
		if err := s.store.RevokeUserLinks(ctx, handle, now); err != nil {
			return nil, fmt.Errorf("user: disable %q: %w", handle, err)
		}
		if s.opts.OnDisable != nil {
			if err := s.opts.OnDisable(ctx, handle); err != nil {
				return nil, fmt.Errorf("user: disable %q: %w", handle, err)
			}
		}
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, u.Handle, details)
	return u, nil
}

// ScopesFor returns the scopes u's role allows.
func ScopesFor(u *User) []string {
	if u.Role == RoleAdmin {
		return slices.Clone(auth.AllScopes)
	}
	return slices.Clone(auth.MemberScopes)
}

// IssueKeyRequest describes a new key.
type IssueKeyRequest struct {
	Handle string
	Label  string
	// Scopes default to auth.MemberScopes, or to those of them the issuing
	// key holds.
	Scopes []string
	// Slugs restricts the key to artifacts whose slug matches a pattern;
	// empty is no restriction, or the issuing key's.
	Slugs []string
	// ExpiresIn <= 0 is no expiry, or Options.MaxKeyTTL, or the issuing
	// key's expiry, whichever ends first.
	ExpiresIn time.Duration
	// With is the prefix of the API key making the request, when a person
	// issues a key for themselves. The new key is then bounded by it: no
	// scope it lacks, never admin, a restriction no wider and an expiry no
	// later; and revoking it revokes the new key. "" is an admin, who is
	// bounded by the owner's role alone, unless Session is set.
	With string
	// Session marks a request a person makes from their own browser session,
	// opened with a password or at the identity provider. No key bounds it,
	// so the role does, short of admin: a stolen session mints no admin
	// key. A session opened with an API key sets With instead.
	Session bool
}

// IssueKey creates an API key and returns it with the key string, which is
// never available again. ErrNotFound, ErrDisabled, ErrInvalidScope,
// ErrInvalidKey, and with With also ErrKeyNotFound and ErrKeyTooWide.
func (s *Service) IssueKey(ctx context.Context, req IssueKeyRequest) (_ *APIKey, _ string, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.issue_key")
	defer func() { endSpan(span, err) }()
	u, err := s.store.GetUser(ctx, req.Handle)
	if err != nil {
		return nil, "", err
	}
	if u.DisabledAt != nil {
		return nil, "", fmt.Errorf("user: issue key for %q: %w", req.Handle, ErrDisabled)
	}
	now := s.now()
	with, err := s.actingKey(ctx, req.Handle, req.With, now)
	if err != nil {
		return nil, "", err
	}

	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = auth.MemberScopes
		if with != nil {
			scopes = intersect(scopes, effectiveScopes(with, u))
		}
	}
	allowed := ScopesFor(u)
	for _, sc := range scopes {
		if !slices.Contains(auth.AllScopes, sc) {
			return nil, "", fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, sc)
		}
		if req.Session && with == nil && sc == auth.ScopeAdmin {
			return nil, "", fmt.Errorf("%w: a browser session issues no key with %q, an admin does", ErrKeyTooWide, sc)
		}
		if !slices.Contains(allowed, sc) {
			return nil, "", fmt.Errorf("%w: role %s may not hold %q", ErrInvalidScope, u.Role, sc)
		}
	}
	granted := intersect(auth.AllScopes, scopes) // canonical order, deduplicated
	if len(granted) == 0 {
		return nil, "", fmt.Errorf("%w: the key would hold no scope", ErrInvalidScope)
	}

	slugs, err := cleanPatterns(req.Slugs)
	if err != nil {
		return nil, "", err
	}
	if with != nil && len(slugs) == 0 {
		slugs = slices.Clone(with.Slugs)
	}
	if len(slugs) > 0 && slices.Contains(granted, auth.ScopeAdmin) {
		return nil, "", fmt.Errorf("%w: a key restricted to slugs may not hold %q", ErrInvalidScope, auth.ScopeAdmin)
	}

	k := &APIKey{
		UserHandle: req.Handle,
		Label:      strings.TrimSpace(req.Label),
		Scopes:     granted,
		Slugs:      slugs,
		CreatedAt:  now,
	}
	if with != nil {
		if slices.Contains(granted, auth.ScopeAdmin) {
			return nil, "", fmt.Errorf("%w: no key issues a key with %q, an admin does", ErrKeyTooWide, auth.ScopeAdmin)
		}
		if err := covers(with, u, k); err != nil {
			return nil, "", err
		}
		k.IssuedBy = with.Prefix
	}
	if k.ExpiresAt, err = s.expiry(req.ExpiresIn, with, now); err != nil {
		return nil, "", err
	}

	key, prefix, err := newKey()
	if err != nil {
		return nil, "", err
	}
	k.Prefix, k.Hash = prefix, HashKey(key)
	if err := s.store.CreateKey(ctx, k, s.opts.MaxKeys); err != nil {
		return nil, "", err
	}
	s.record(ctx, actorFrom(ctx, req.Handle), audit.KeyIssue, k.UserHandle, keyDetails(k))
	return k, key, nil
}

// expiry is when a key issued now for ttl ends: ttl <= 0 asks for none.
// Options.MaxKeyTTL caps it, and so does the expiry of the key it is issued
// with; a longer ttl asked for by name is refused, not shortened.
func (s *Service) expiry(ttl time.Duration, with *APIKey, now time.Time) (*time.Time, error) {
	limit := s.opts.MaxKeyTTL
	if limit > 0 && ttl > limit {
		return nil, fmt.Errorf("%w: a key may live at most %s on this instance", ErrInvalidKey, formatTTL(limit))
	}
	asked := ttl > 0
	if !asked {
		ttl = limit
	}
	var exp *time.Time
	if ttl > 0 {
		t := now.Add(ttl)
		exp = &t
	}
	if with == nil || with.ExpiresAt == nil {
		return exp, nil
	}
	if exp == nil || exp.After(*with.ExpiresAt) {
		if asked {
			return nil, fmt.Errorf("%w: it expires at %s, before the new key would",
				ErrKeyTooWide, with.ExpiresAt.UTC().Format(time.RFC3339))
		}
		t := *with.ExpiresAt
		exp = &t
	}
	return exp, nil
}

// actingKey loads the key a person makes a request with: one of handle's
// own, still active. A nil key for an empty prefix. ErrKeyNotFound.
func (s *Service) actingKey(ctx context.Context, handle, prefix string, now time.Time) (*APIKey, error) {
	if prefix == "" {
		return nil, nil
	}
	k, u, err := s.store.GetKey(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if u.Handle != handle || !keyActive(k, u, now) {
		return nil, fmt.Errorf("user: key %q of %q: %w", prefix, handle, ErrKeyNotFound)
	}
	return k, nil
}

// effectiveScopes are the scopes k acts with: its own, capped at the role
// its owner has now.
func effectiveScopes(k *APIKey, u *User) []string {
	return intersect(k.Scopes, ScopesFor(u))
}

// covers reports ErrKeyTooWide unless target holds no scope and reaches no
// slug that with does not, so that acting on target with with gives its
// holder nothing new.
func covers(with *APIKey, u *User, target *APIKey) error {
	held := effectiveScopes(with, u)
	for _, sc := range target.Scopes {
		if !slices.Contains(held, sc) {
			return fmt.Errorf("%w: it does not hold %q", ErrKeyTooWide, sc)
		}
	}
	if !artifact.SlugPatternsWithin(target.Slugs, with.Slugs) {
		return fmt.Errorf("%w: it is restricted to slugs matching %s", ErrKeyTooWide, strings.Join(with.Slugs, ", "))
	}
	return nil
}

func cleanPatterns(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if err := artifact.ValidateSlugPatterns(out); err != nil {
		if ae, ok := errors.AsType[*artifact.Error](err); ok {
			return nil, fmt.Errorf("%w: slugs: %s", ErrInvalidKey, ae.Message)
		}
		return nil, fmt.Errorf("%w: slugs: %w", ErrInvalidKey, err)
	}
	return out, nil
}

func keyDetails(k *APIKey) map[string]string {
	d := map[string]string{
		"handle": k.UserHandle, "prefix": k.Prefix, "label": k.Label, "scopes": strings.Join(k.Scopes, " "),
	}
	if len(k.Slugs) > 0 {
		d["slugs"] = strings.Join(k.Slugs, " ")
	}
	if k.IssuedBy != "" {
		d["issued_by"] = k.IssuedBy
	}
	if k.ExpiresAt != nil {
		d["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return d
}

// formatTTL prints a whole number of days as "90d", anything else as a Go
// duration.
func formatTTL(d time.Duration) string {
	const day = 24 * time.Hour
	if d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	return d.String()
}

func intersect(a, b []string) []string {
	var out []string
	for _, s := range a {
		if slices.Contains(b, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// ListKeys returns the user's keys, newest first.
func (s *Service) ListKeys(ctx context.Context, handle string) ([]APIKey, error) {
	return s.store.ListKeys(ctx, handle)
}

// KeyRef names one of a user's keys for a change. With is the prefix of the
// API key making the request, when a person acts on their own keys: the key
// named must then be no wider than it. "" is an admin, who is not bounded,
// or with Session a person in their own browser session, who revokes any of
// their keys and rotates any but one that holds admin.
type KeyRef struct {
	Handle  string
	Prefix  string
	With    string
	Session bool
}

// target loads the key ref names and, for a person's request, checks the
// key they make it with covers it. ErrKeyNotFound, ErrKeyTooWide.
func (s *Service) target(ctx context.Context, ref KeyRef, now time.Time) (k, with *APIKey, u *User, err error) {
	k, u, err = s.store.GetKey(ctx, ref.Prefix)
	if err != nil {
		return nil, nil, nil, err
	}
	if u.Handle != ref.Handle {
		return nil, nil, nil, fmt.Errorf("user: key %q of %q: %w", ref.Prefix, ref.Handle, ErrKeyNotFound)
	}
	if with, err = s.actingKey(ctx, ref.Handle, ref.With, now); err != nil {
		return nil, nil, nil, err
	}
	if with != nil && with.Prefix != k.Prefix {
		if err := covers(with, u, k); err != nil {
			return nil, nil, nil, err
		}
	}
	return k, with, u, nil
}

// RevokeKey revokes one of the user's keys and every key issued with it.
// ErrKeyNotFound, ErrKeyTooWide.
func (s *Service) RevokeKey(ctx context.Context, ref KeyRef) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.revoke_key")
	defer func() { endSpan(span, err) }()
	now := s.now()
	if _, _, _, err := s.target(ctx, ref, now); err != nil {
		return err
	}
	issued, err := s.store.RevokeKey(ctx, ref.Handle, ref.Prefix, now)
	if err != nil {
		return err
	}
	actor := actorFrom(ctx, ref.Handle)
	s.record(ctx, actor, audit.KeyRevoke, ref.Handle, map[string]string{"handle": ref.Handle, "prefix": ref.Prefix})
	for _, p := range issued {
		s.record(ctx, actor, audit.KeyRevoke, ref.Handle, map[string]string{"handle": ref.Handle, "prefix": p, "issued_by": ref.Prefix})
	}
	return nil
}

const (
	// DefaultRotateOverlap is how long a rotated key keeps working when the
	// request names no overlap.
	DefaultRotateOverlap = 24 * time.Hour
	// MaxRotateOverlap bounds it: a longer one would leave the old key in
	// use, which is what a rotation ends.
	MaxRotateOverlap = 7 * 24 * time.Hour
)

// RotateKey replaces a key: it issues one with the same owner, label,
// scopes, restriction, issuer and lifetime, and sets the old key to expire
// when overlap has passed (0 ends it now), so both work in between. It
// returns the new key with its key string, and the old key as it now is.
// Options.MaxKeyTTL caps the new key's lifetime, and for a person's request
// so does the expiry of the key they make it with, itself included: a key
// renews its secret, never its own time. ErrKeyNotFound for a key that is no
// longer active, ErrKeyTooWide, ErrInvalidKey for an overlap out of range.
func (s *Service) RotateKey(ctx context.Context, ref KeyRef, overlap time.Duration) (_ *APIKey, _ string, _ *APIKey, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.rotate_key")
	defer func() { endSpan(span, err) }()
	if overlap < 0 || overlap > MaxRotateOverlap {
		return nil, "", nil, fmt.Errorf("%w: overlap must be between 0 and %s", ErrInvalidKey, formatTTL(MaxRotateOverlap))
	}
	now := s.now()
	old, with, u, err := s.target(ctx, ref, now)
	if err != nil {
		return nil, "", nil, err
	}
	if !keyActive(old, u, now) {
		return nil, "", nil, fmt.Errorf("user: rotate key %q: it is revoked or expired: %w", ref.Prefix, ErrKeyNotFound)
	}
	if (with != nil || ref.Session) && slices.Contains(old.Scopes, auth.ScopeAdmin) {
		return nil, "", nil, fmt.Errorf("%w: a key with %q is rotated on the admin API alone", ErrKeyTooWide, auth.ScopeAdmin)
	}

	k := &APIKey{
		UserHandle: old.UserHandle,
		Label:      old.Label,
		Scopes:     effectiveScopes(old, u),
		Slugs:      slices.Clone(old.Slugs),
		IssuedBy:   old.IssuedBy,
		CreatedAt:  now,
	}
	var ttl time.Duration
	if old.ExpiresAt != nil {
		ttl = old.ExpiresAt.Sub(old.CreatedAt)
	}
	if limit := s.opts.MaxKeyTTL; limit > 0 && (ttl <= 0 || ttl > limit) {
		ttl = limit
	}
	if ttl > 0 {
		t := now.Add(ttl)
		k.ExpiresAt = &t
	}
	if with != nil && with.ExpiresAt != nil && (k.ExpiresAt == nil || k.ExpiresAt.After(*with.ExpiresAt)) {
		t := *with.ExpiresAt
		k.ExpiresAt = &t
	}

	key, prefix, err := newKey()
	if err != nil {
		return nil, "", nil, err
	}
	k.Prefix, k.Hash = prefix, HashKey(key)
	ends := now.Add(overlap)
	if err := s.store.RotateKey(ctx, old.Prefix, k, ends, s.opts.MaxKeys); err != nil {
		return nil, "", nil, err
	}
	if old.ExpiresAt == nil || ends.Before(*old.ExpiresAt) {
		old.ExpiresAt = &ends
	}
	details := keyDetails(k)
	details["prefix"], details["new_prefix"] = old.Prefix, k.Prefix
	details["old_expires_at"] = old.ExpiresAt.UTC().Format(time.RFC3339)
	s.record(ctx, actorFrom(ctx, ref.Handle), audit.KeyRotate, k.UserHandle, details)
	return k, key, old, nil
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
		Role:       string(u.Role),
		Scopes:     scopes,
		Source:     auth.SourceAPIKey,
		KeyID:      k.Prefix,
		QuotaBytes: u.QuotaBytes,
		Slugs:      k.Slugs,
		ExpiresAt:  k.ExpiresAt,
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
	return KeyStatus{
		Handle: u.Handle, Active: keyActive(k, u, s.now()), ExpiresAt: k.ExpiresAt, Slugs: k.Slugs, Scopes: effectiveScopes(k, u),
	}, nil
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
// allowed (any domain when AllowedDomains is empty) or the email is in
// AdminEmails and, for an admin, the email is in AdminEmails; a new admin
// when the email is in AdminEmails; a new member when AutoProvision is on
// and the domain is listed; else ErrNotProvisioned. A
// disabled user yields ErrDisabled. Success refreshes the link's
// last_login_at.
func (s *Service) ResolveIdentity(ctx context.Context, id auth.Identity) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.resolve_identity")
	defer func() { endSpan(span, err) }()
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

	listedAdmin := s.adminEmail(email)
	if len(s.opts.AllowedDomains) == 0 || domainListed || listedAdmin {
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

	role := RoleMember
	switch {
	case listedAdmin:
		role = RoleAdmin
	case !s.opts.AutoProvision || !domainListed:
		return nil, ErrNotProvisioned
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
func (s *Service) LinkIdentity(ctx context.Context, handle, issuer, subject, email string) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.link_identity")
	defer func() { endSpan(span, err) }()
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ErrInvalidIdentity
	}
	err = s.store.LinkIdentity(ctx, handle, Identity{
		Issuer:       issuer,
		Subject:      subject,
		EmailAtLogin: normalizeEmail(email),
	})
	if err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, handle, map[string]string{
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
	s.record(ctx, actorFrom(ctx, "admin"), audit.UserUpdate, handle, map[string]string{
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
		s.record(ctx, u.Handle, audit.UserCreate, u.Handle, map[string]string{
			"handle": u.Handle, "email": u.Email, "role": string(u.Role),
			"quota_bytes": strconv.FormatInt(u.QuotaBytes, 10), "via": "identity",
		})
		return u, nil
	}
	return nil, fmt.Errorf("user: provision %q: no free handle after %d attempts: %w",
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

// record appends an event about the account of subject.
func (s *Service) record(ctx context.Context, actor, action, subject string, details map[string]string) {
	// Best-effort: see Service.
	_ = s.audit.Record(ctx, audit.Event{At: s.now(), Actor: actor, Action: action, Subject: subject, Details: details})
}
