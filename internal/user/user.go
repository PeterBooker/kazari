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
	// ErrLastAdmin is a change that would leave the instance without an
	// active admin.
	ErrLastAdmin = errors.New("user: the last active admin cannot be disabled or demoted")
	// ErrSelf is a person disabling or demoting their own account.
	ErrSelf = errors.New("user: nobody disables or demotes their own account")
	// ErrTooManyKeys is a key refused because its owner holds
	// Options.MaxKeys live ones.
	ErrTooManyKeys = errors.New("user: too many live keys")
	// ErrBadPassword is a refused sign-in: an unknown name, an account
	// without a password, a disabled one or a wrong password, never told
	// apart.
	ErrBadPassword = errors.New("user: wrong name or password")
	// ErrWeakPassword is a new password outside the length limits.
	ErrWeakPassword = errors.New("user: password not accepted")
	// ErrLinkNotFound is a link that sets no password: unknown, used,
	// replaced, revoked or expired, or its person disabled.
	ErrLinkNotFound = errors.New("user: link not found")
	// ErrSetupDone is a first-run setup asked of an instance that has users.
	ErrSetupDone = errors.New("user: the instance is already set up")
	// ErrLocalOff is a password or a link asked of an instance whose people
	// sign in elsewhere (Options.LocalAccounts is off).
	ErrLocalOff = errors.New("user: built-in accounts are off on this instance")
)

// Purpose is what a Link is for.
type Purpose string

const (
	PurposeInvite Purpose = "invite" // a new person sets their first password
	PurposeReset  Purpose = "reset"  // a person sets a new password
	PurposeSetup  Purpose = "setup"  // the first admin of a fresh instance is created
)

// Link is a single-use link that sets a password. Its token is shown once,
// when the link is made; only Hash (hex SHA-256 of the token) is stored.
type Link struct {
	ID         int64
	UserHandle string // "" for PurposeSetup
	Purpose    Purpose
	Hash       string
	// RevokeAll makes using a reset link revoke the person's keys and OAuth
	// grants too.
	RevokeAll bool
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

// Live reports whether the link can still be used at now.
func (l *Link) Live(now time.Time) bool {
	return l.UsedAt == nil && l.RevokedAt == nil && now.Before(l.ExpiresAt)
}

// Account is how a person signs in and when they last did.
type Account struct {
	Handle      string
	HasPassword bool
	// Invited is a person who was sent an invite and has not set a password.
	Invited         bool
	Identities      int
	LiveKeys        int
	PasswordLoginAt *time.Time
	IdentityLoginAt *time.Time
	KeyUsedAt       *time.Time
}

// LastActive is the latest of the last password sign-in, sign-in at the
// identity provider and key use; nil when there is none.
func (a Account) LastActive() *time.Time {
	var last *time.Time
	for _, t := range []*time.Time{a.PasswordLoginAt, a.IdentityLoginAt, a.KeyUsedAt} {
		if t != nil && (last == nil || t.After(*last)) {
			last = t
		}
	}
	return last
}

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
	Scopes    []string   // what the key acts with now: its scopes, capped at its owner's role
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
	// ErrLastAdmin, with nothing written, when the user is an active admin,
	// u no longer is and no other active admin exists, checked in the same
	// transaction so that two changes cannot each rely on the other's admin.
	UpdateUser(ctx context.Context, u *User) error

	// CreateKey inserts k and sets k.ID. ErrKeyNotFound when k.IssuedBy
	// names no key of the same user. ErrTooManyKeys, with nothing written,
	// when maxLive > 0 and the user already holds that many keys that are
	// neither revoked nor expired at k.CreatedAt.
	CreateKey(ctx context.Context, k *APIKey, maxLive int) error
	// RotateKey is CreateKey for the successor of the user's key with
	// prefix, which in the same transaction is set to expire at expires
	// unless it ends sooner. ErrKeyNotFound.
	RotateKey(ctx context.Context, prefix string, k *APIKey, expires time.Time, maxLive int) error
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

	// PasswordHash returns the user's password hash, or "" when the user
	// has no password or does not exist.
	PasswordHash(ctx context.Context, handle string) (string, error)
	// SetPassword stores hash as the user's password. ErrNotFound.
	SetPassword(ctx context.Context, handle, hash string, at time.Time) error
	// TouchPassword records a sign-in with the password. A user without a
	// password is not an error.
	TouchPassword(ctx context.Context, handle string, at time.Time) error

	// CreateLink inserts l, sets l.ID and revokes, at l.CreatedAt, every
	// earlier link of the same person and purpose that is still unused, so
	// one link per person and purpose works at a time. ErrNotFound for an
	// unknown handle. A PurposeSetup link names no user and is refused with
	// ErrSetupDone once any user exists.
	CreateLink(ctx context.Context, l *Link) error
	// GetLink returns the link whose token hashes to hash, in any state,
	// with its user (nil for PurposeSetup). ErrLinkNotFound.
	GetLink(ctx context.Context, hash string) (*Link, *User, error)
	// ListLinks returns the links that are live at now, newest first.
	ListLinks(ctx context.Context, now time.Time) ([]Link, error)
	// RevokeLink revokes the link with id if it is live at the given time,
	// and returns it. ErrLinkNotFound otherwise.
	RevokeLink(ctx context.Context, id int64, at time.Time) (*Link, error)
	// UseLink marks the link used at the given time and stores
	// passwordHash for its user, in one transaction, and returns both.
	// ErrLinkNotFound unless the link is live then and its user enabled.
	// For a PurposeSetup link it first inserts first as a new user, and
	// answers ErrSetupDone, with nothing written, if any user exists: of
	// two people racing for a fresh instance one becomes its admin.
	// ErrHandleTaken and ErrEmailTaken cannot occur on an empty table.
	UseLink(ctx context.Context, hash string, at time.Time, passwordHash string, first *User) (*Link, *User, error)

	// ListAccounts returns every user's Account, ordered by handle, with
	// keys counted as live at now.
	ListAccounts(ctx context.Context, now time.Time) ([]Account, error)
}

// Count is how many users have a role and disabled state.
type Count struct {
	Role     Role
	Disabled bool
	Count    int64
}
