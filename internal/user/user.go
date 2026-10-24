// Package user holds users and their API keys: creating users, issuing and
// verifying keys, and resolving an upstream identity to a user under the
// instance's provisioning policy. Service implements auth.Authenticator.
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

// Errors. Compare with errors.Is.
var (
	ErrNotFound       = errors.New("user: not found")
	ErrKeyNotFound    = errors.New("user: key not found")
	ErrHandleTaken    = errors.New("user: handle is taken")
	ErrEmailTaken     = errors.New("user: email is taken")
	ErrInvalidHandle  = errors.New("user: invalid handle")
	ErrInvalidRole    = errors.New("user: invalid role")
	ErrInvalidScope   = errors.New("user: invalid scope")
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
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// KeyStatus is the state of an API key at one moment, for credentials
// derived from the key (an OAuth grant approved with it).
type KeyStatus struct {
	Handle    string     // the key's owner
	Active    bool       // neither revoked nor expired, and the owner is not disabled
	ExpiresAt *time.Time // nil when the key never expires
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

	// CreateKey inserts k and sets k.ID.
	CreateKey(ctx context.Context, k *APIKey) error
	// GetKey returns the key and its user by prefix, including revoked and
	// expired keys; the caller decides. ErrKeyNotFound.
	GetKey(ctx context.Context, prefix string) (*APIKey, *User, error)
	// ListKeys returns a user's keys, newest first.
	ListKeys(ctx context.Context, handle string) ([]APIKey, error)
	// RevokeKey sets revoked_at on one key. ErrKeyNotFound.
	RevokeKey(ctx context.Context, handle, prefix string, at time.Time) error
	// RevokeAllKeys revokes every live key of a user (on disable).
	RevokeAllKeys(ctx context.Context, handle string, at time.Time) error
	// TouchKey sets last_used_at.
	TouchKey(ctx context.Context, prefix string, at time.Time) error

	// GetIdentity returns the user linked to (issuer, subject). ErrNotFound.
	GetIdentity(ctx context.Context, issuer, subject string) (*User, error)
	// LinkIdentity upserts the link, refreshing email_at_login and
	// last_login_at.
	LinkIdentity(ctx context.Context, handle string, id Identity) error
	UnlinkIdentity(ctx context.Context, handle, issuer, subject string) error
	ListIdentities(ctx context.Context, handle string) ([]Identity, error)
}
