// Package user manages users, API keys and identity provisioning. Service
// implements auth.Authenticator.
package user

import (
	"context"
	"errors"
	"time"
)

// Role of a user.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// KeyPrefix starts every API key: kzk_<8-char prefix>_<secret>.
const KeyPrefix = "kzk_"

var (
	ErrNotFound      = errors.New("user: not found")
	ErrKeyNotFound   = errors.New("user: key not found")
	ErrHandleTaken   = errors.New("user: handle is taken")
	ErrEmailTaken    = errors.New("user: email is taken")
	ErrInvalidHandle = errors.New("user: invalid handle")
	ErrInvalidRole   = errors.New("user: invalid role")
	ErrInvalidScope  = errors.New("user: invalid scope")
	// ErrInvalidKey is a request for a key that cannot exist: a malformed
	// slug pattern, a lifetime over the limit.
	ErrInvalidKey = errors.New("user: invalid key")
	// ErrKeyTooWide is a person's key asked to issue, rotate or revoke a key
	// that holds more than it does.
	ErrKeyTooWide     = errors.New("user: the key making the request is too narrow")
	ErrDisabled       = errors.New("user: disabled")
	ErrNotProvisioned = errors.New("user: identity is not allowed on this instance")
)

// User is a member or admin of the instance.
type User struct {
	ID          int64
	Handle      string
	Email       string // "" when unknown
	DisplayName string
	Role        Role
	QuotaBytes  int64 // 0 = unlimited
	DisabledAt  *time.Time
	CreatedAt   time.Time
}

// APIKey is a bearer credential. The secret is shown once at issue time;
// only Hash (hex SHA-256 of the whole key string) is stored.
type APIKey struct {
	ID         int64
	UserHandle string
	Prefix     string
	Hash       string
	Label      string
	Scopes     []string
	// Slugs restricts the key to artifacts whose slug matches one of these
	// patterns (artifact.MatchSlug); empty is no restriction. Set at issue
	// and never changed.
	Slugs []string
	// IssuedBy is the prefix of the key its owner issued it with; "" when an
	// admin issued it. Revoking that key revokes this one.
	IssuedBy   string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// KeyStatus is the state of an API key at one moment, for credentials
// derived from the key (an OAuth grant approved with it).
type KeyStatus struct {
	Handle    string
	Active    bool       // neither revoked nor expired, and the owner is not disabled
	ExpiresAt *time.Time // nil when the key never expires
	Slugs     []string   // the key's restriction, empty when it has none
}

// Identity links an upstream (issuer, subject) pair to a user.
type Identity struct {
	Issuer       string
	Subject      string
	EmailAtLogin string
	LastLoginAt  time.Time
}

// Store is the persistence contract. Handles are the external key.
type Store interface {
	// CreateUser inserts u and sets u.ID. ErrHandleTaken, ErrEmailTaken.
	CreateUser(ctx context.Context, u *User) error
	GetUser(ctx context.Context, handle string) (*User, error)
	// GetUserByEmail matches case-insensitively. ErrNotFound.
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	// ListUsers returns every user ordered by handle.
	ListUsers(ctx context.Context) ([]User, error)
	// UpdateUser writes email, display name, role, quota and disabled_at.
	UpdateUser(ctx context.Context, u *User) error

	// CreateKey inserts k and sets k.ID. ErrKeyNotFound when k.IssuedBy
	// names no key of the same user.
	CreateKey(ctx context.Context, k *APIKey) error
	// RotateKey is CreateKey for the successor of the user's key with
	// prefix, which in the same transaction is set to expire at expires
	// unless it ends sooner. ErrKeyNotFound.
	RotateKey(ctx context.Context, prefix string, k *APIKey, expires time.Time) error
	// GetKey returns the key and its user by prefix, including revoked and
	// expired keys; the caller decides. ErrKeyNotFound.
	GetKey(ctx context.Context, prefix string) (*APIKey, *User, error)
	// ListKeys returns a user's keys, newest first. ErrNotFound.
	ListKeys(ctx context.Context, handle string) ([]APIKey, error)
	// RevokeKey sets revoked_at on one key and on every key issued with it,
	// directly or through another, unless already set. It returns the
	// prefixes of the issued keys it revoked. ErrKeyNotFound.
	RevokeKey(ctx context.Context, handle, prefix string, at time.Time) (issued []string, err error)
	// RevokeAllKeys revokes every live key of a user. ErrNotFound.
	RevokeAllKeys(ctx context.Context, handle string, at time.Time) error
	TouchKey(ctx context.Context, prefix string, at time.Time) error

	// GetIdentity returns the user linked to (issuer, subject). ErrNotFound.
	GetIdentity(ctx context.Context, issuer, subject string) (*User, error)
	// LinkIdentity upserts the link, moving it from any other user and
	// refreshing email_at_login and last_login_at. ErrNotFound.
	LinkIdentity(ctx context.Context, handle string, id Identity) error
	UnlinkIdentity(ctx context.Context, handle, issuer, subject string) error
	ListIdentities(ctx context.Context, handle string) ([]Identity, error)
	// CountUsers counts users by role and whether they are disabled.
	CountUsers(ctx context.Context) ([]Count, error)
}

// Count is how many users have a role and disabled state.
type Count struct {
	Role     Role
	Disabled bool
	Count    int64
}
