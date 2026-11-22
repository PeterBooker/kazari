package user

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// Built-in accounts: a password per person, and the single-use links that
// set one. Nothing is ever sent by email: an admin copies a link and hands
// it over.

// LinkPrefix starts every link token: kzl_<secret>.
const LinkPrefix = "kzl_"

const (
	DefaultMinPasswordLength = 15
	DefaultInviteTTL         = 7 * 24 * time.Hour
	DefaultResetTTL          = 24 * time.Hour
	DefaultSetupTTL          = time.Hour
	// DefaultMaxHashes at DefaultArgon2 holds a quarter of a 1 GiB host.
	DefaultMaxHashes = 4
)

var linkTokenLen = len(LinkPrefix) + keyEncoding.EncodedLen(secretBytes)

func newLinkToken() (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("user: generate link: %w", err)
	}
	return LinkPrefix + keyEncoding.EncodeToString(b[:]), nil
}

// IsLinkToken reports whether token has the shape of a link token, so a
// malformed one is refused without a query.
func IsLinkToken(token string) bool {
	rest, ok := strings.CutPrefix(token, LinkPrefix)
	return ok && len(token) == linkTokenLen && isBase32(rest)
}

// InviteRequest describes a person to invite.
type InviteRequest struct {
	Handle      string
	Email       string
	DisplayName string
	Role        Role   // "" = RoleMember
	QuotaBytes  *int64 // nil = Options.DefaultQuota
}

// Invite creates a user without a credential and the invite link that lets
// them set a password. It returns the link's token, which is never
// available again. Only an admin invites an admin. The errors of
// CreateUser, and ErrLocalOff.
func (s *Service) Invite(ctx context.Context, req InviteRequest) (_ *User, _ *Link, token string, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.invite")
	defer func() { endSpan(span, err) }()
	if !s.opts.LocalAccounts {
		return nil, nil, "", ErrLocalOff
	}
	// The caller's own rights, not the route's: every route that invites
	// passes through here.
	if req.Role == RoleAdmin {
		if p, ok := auth.FromContext(ctx); !ok || !p.Admin {
			return nil, nil, "", fmt.Errorf("%w: only an admin invites an admin", ErrInvalidRole)
		}
	}
	u, err := s.newUser(CreateUserRequest(req))
	if err != nil {
		return nil, nil, "", err
	}
	l, token, err := s.buildLink(ctx, u.Handle, PurposeInvite, false)
	if err != nil {
		return nil, nil, "", err
	}
	if err := s.store.CreateInvited(ctx, u, l); err != nil {
		return nil, nil, "", err
	}
	s.recordCreated(ctx, u)
	s.linkCreated(ctx, l)
	return u, l, token, nil
}

// IssueLink makes a link that lets handle set a password, ending any earlier
// link of the same purpose: PurposeInvite replaces an invite that was lost
// or has lapsed, PurposeReset is for a person who has a password or lost
// it. With revokeAll, using a reset link also revokes the person's keys,
// recovery codes, passkeys and OAuth grants. ErrNotFound, ErrDisabled,
// ErrLocalOff.
func (s *Service) IssueLink(ctx context.Context, handle string, purpose Purpose, revokeAll bool) (_ *Link, token string, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.issue_link")
	defer func() { endSpan(span, err) }()
	if purpose != PurposeInvite && purpose != PurposeReset {
		return nil, "", fmt.Errorf("user: issue link: unknown purpose %q", purpose)
	}
	u, err := s.store.GetUser(ctx, handle)
	if err != nil {
		return nil, "", err
	}
	if u.DisabledAt != nil {
		return nil, "", fmt.Errorf("user: issue link for %q: %w", handle, ErrDisabled)
	}
	if purpose == PurposeInvite {
		// An invite ends no session; for a person who has signed in, a new
		// password has to, which is what a reset does.
		hash, err := s.store.PasswordHash(ctx, handle)
		if err != nil {
			return nil, "", err
		}
		if hash != "" {
			return nil, "", fmt.Errorf("user: invite %q: %w; make a reset link instead", handle, ErrHasPassword)
		}
	}
	return s.newLink(ctx, handle, purpose, revokeAll && purpose == PurposeReset)
}

// SetupLink makes the link that creates the first admin of a fresh
// instance, ending any earlier one. ErrSetupDone once a user exists,
// ErrLocalOff.
func (s *Service) SetupLink(ctx context.Context) (_ *Link, token string, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.setup_link")
	defer func() { endSpan(span, err) }()
	return s.newLink(ctx, "", PurposeSetup, false)
}

func (s *Service) newLink(ctx context.Context, handle string, purpose Purpose, revokeAll bool) (*Link, string, error) {
	l, token, err := s.buildLink(ctx, handle, purpose, revokeAll)
	if err != nil {
		return nil, "", err
	}
	if err := s.store.CreateLink(ctx, l); err != nil {
		return nil, "", err
	}
	s.linkCreated(ctx, l)
	return l, token, nil
}

// buildLink returns a link with a new token, not yet stored.
func (s *Service) buildLink(ctx context.Context, handle string, purpose Purpose, revokeAll bool) (*Link, string, error) {
	if !s.opts.LocalAccounts {
		return nil, "", ErrLocalOff
	}
	token, err := newLinkToken()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	return &Link{
		UserHandle: handle,
		Purpose:    purpose,
		Hash:       HashKey(token),
		RevokeAll:  revokeAll,
		CreatedBy:  actorFrom(ctx, "admin"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.linkTTL(purpose)),
	}, token, nil
}

func (s *Service) linkCreated(ctx context.Context, l *Link) {
	s.link(ctx, linkCreate, l.Purpose)
	s.record(ctx, l.CreatedBy, audit.InviteCreate, l.UserHandle, linkDetails(l))
}

func (s *Service) linkTTL(p Purpose) time.Duration {
	switch p {
	case PurposeInvite:
		return s.opts.InviteTTL
	case PurposeReset:
		return s.opts.ResetTTL
	default:
		return s.opts.SetupTTL
	}
}

func linkDetails(l *Link) map[string]string {
	d := map[string]string{
		"id": strconv.FormatInt(l.ID, 10), "purpose": string(l.Purpose),
		"expires_at": l.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if l.UserHandle != "" {
		d["handle"] = l.UserHandle
	}
	if l.RevokeAll {
		d["revoke_all"] = "true"
	}
	return d
}

// Links returns the links that can still be used, newest first.
func (s *Service) Links(ctx context.Context) ([]Link, error) {
	return s.store.ListLinks(ctx, s.now())
}

// RevokeLink ends a link that has not been used. ErrLinkNotFound.
func (s *Service) RevokeLink(ctx context.Context, id int64) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.revoke_link")
	defer func() { endSpan(span, err) }()
	l, err := s.store.RevokeLink(ctx, id, s.now())
	if err != nil {
		return err
	}
	s.link(ctx, linkRevoke, l.Purpose)
	s.record(ctx, actorFrom(ctx, "admin"), audit.InviteRevoke, l.UserHandle, linkDetails(l))
	return nil
}

// Link returns the live link token names and the person it is for (nil for
// PurposeSetup), and changes nothing: the chat applications links are
// pasted into fetch them for a preview. ErrLinkNotFound for a link that is
// malformed, unknown, used, revoked or expired, or whose person is
// disabled; ErrSetupDone for a setup link once the instance has users.
func (s *Service) Link(ctx context.Context, token string) (*Link, *User, error) {
	if !s.opts.LocalAccounts || !IsLinkToken(token) {
		return nil, nil, ErrLinkNotFound
	}
	l, u, err := s.store.GetLink(ctx, HashKey(token))
	if err != nil {
		return nil, nil, err
	}
	if !l.Live(s.now()) || (u != nil && u.DisabledAt != nil) {
		return nil, nil, ErrLinkNotFound
	}
	if l.Purpose == PurposeSetup {
		counts, err := s.store.CountUsers(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(counts) > 0 {
			return nil, nil, ErrSetupDone
		}
	}
	return l, u, nil
}

// RedeemRequest is what a person enters on the page a link opens.
type RedeemRequest struct {
	Password string
	// Handle, Email and DisplayName describe the first admin, for a
	// PurposeSetup link only.
	Handle      string
	Email       string
	DisplayName string
}

// Redeem uses the link token names up and sets the person's password; a
// PurposeSetup link creates the first admin with it. It does not sign
// anyone in. A reset ends the person's browser sessions, and with
// Link.RevokeAll their keys, recovery codes, passkeys and OAuth grants.
// ErrLinkNotFound and ErrSetupDone as for Link; ErrWeakPassword and, for
// setup, ErrInvalidHandle, with the link left unused.
func (s *Service) Redeem(ctx context.Context, token string, req RedeemRequest) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.redeem_link")
	defer func() { endSpan(span, err) }()
	l, _, err := s.Link(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := checkPolicy(req.Password, s.opts.MinPasswordLength); err != nil {
		return nil, err
	}
	now := s.now()
	var first *User
	if l.Purpose == PurposeSetup {
		if err := ValidateHandle(req.Handle); err != nil {
			return nil, err
		}
		first = &User{
			Handle:      req.Handle,
			Email:       normalizeEmail(req.Email),
			DisplayName: strings.TrimSpace(req.DisplayName),
			Role:        RoleAdmin,
			QuotaBytes:  s.opts.DefaultQuota,
			CreatedAt:   now,
		}
	}
	// Hashed only for a link that is live, so a made-up token costs a query
	// and not 64 MiB.
	hash, err := s.hashing.hash(ctx, req.Password)
	if err != nil {
		return nil, err
	}
	l, u, err := s.store.UseLink(ctx, l.Hash, now, hash, first)
	if err != nil {
		return nil, err
	}

	if first != nil {
		s.record(ctx, u.Handle, audit.UserCreate, u.Handle, map[string]string{
			"handle": u.Handle, "email": u.Email, "role": string(u.Role),
			"quota_bytes": strconv.FormatInt(u.QuotaBytes, 10), "via": "setup",
		})
	}
	s.link(ctx, linkUse, l.Purpose)
	s.record(ctx, u.Handle, audit.InviteUse, u.Handle, linkDetails(l))
	s.record(ctx, u.Handle, audit.PasswordSet, u.Handle, map[string]string{"handle": u.Handle, "via": string(l.Purpose)})
	if l.Purpose != PurposeReset {
		return u, nil
	}
	// The password is set and the link used whatever follows: an error here
	// tells the person to ask again, and the next reset repeats the
	// revocation.
	if l.RevokeAll {
		if err := s.store.RevokeAllKeys(ctx, u.Handle, now); err != nil {
			return nil, fmt.Errorf("user: reset %q: %w", u.Handle, err)
		}
		if err := s.store.DeleteRecoveryCodes(ctx, u.Handle); err != nil {
			return nil, fmt.Errorf("user: reset %q: %w", u.Handle, err)
		}
		if err := s.store.DeletePasskeys(ctx, u.Handle); err != nil {
			return nil, fmt.Errorf("user: reset %q: %w", u.Handle, err)
		}
		if s.opts.RevokeGrants != nil {
			if err := s.opts.RevokeGrants(ctx, u.Handle); err != nil {
				return nil, fmt.Errorf("user: reset %q: %w", u.Handle, err)
			}
		}
		return u, nil
	}
	if err := s.endSessions(ctx, u.Handle, 0); err != nil {
		return nil, fmt.Errorf("user: reset %q: %w", u.Handle, err)
	}
	return u, nil
}

func (s *Service) endSessions(ctx context.Context, handle string, keep int64) error {
	if s.opts.EndSessions == nil {
		return nil
	}
	return s.opts.EndSessions(ctx, handle, keep)
}

// CheckPassword signs a person in by handle or email and password, and
// returns them. Every refusal is ErrBadPassword and costs one hash, whether
// the name is unknown, the account has no password or is disabled, or the
// password is wrong. A hash made with other parameters than Options.Argon2
// is renewed.
func (s *Service) CheckPassword(ctx context.Context, login, password string) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.check_password")
	defer func() { endSpan(span, err) }()
	u, stale, err := s.verify(ctx, login, password)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if stale {
		// Best effort: the old hash keeps working until a later sign-in
		// renews it.
		if renewed, err := s.hashing.hash(ctx, password); err == nil {
			_ = s.store.SetPassword(ctx, u.Handle, renewed, now)
		}
	}
	_ = s.store.TouchPassword(ctx, u.Handle, now)
	return u, nil
}

// verify is the check of CheckPassword without what a sign-in adds: it
// renews no hash and records no sign-in. stale reports a hash made with
// other parameters than Options.Argon2.
func (s *Service) verify(ctx context.Context, login, password string) (_ *User, stale bool, err error) {
	if !s.opts.LocalAccounts {
		return nil, false, ErrBadPassword
	}
	u, err := s.byLogin(ctx, login)
	if err != nil {
		return nil, false, err
	}
	var hash string
	if u != nil && u.DisabledAt == nil {
		if hash, err = s.store.PasswordHash(ctx, u.Handle); err != nil {
			return nil, false, err
		}
	}
	// An over-long password cannot be anyone's, and is not worth hashing.
	if len(password) > maxPasswordBytes {
		hash, password = "", ""
	}
	ok, stale, err := s.hashing.verify(ctx, hash, password)
	if err != nil {
		return nil, false, fmt.Errorf("user: check password: %w", err)
	}
	if !ok {
		return nil, false, ErrBadPassword
	}
	return u, stale, nil
}

// HandleFor returns the handle of the person login names, by handle or by
// email, or "" when it names nobody. It is for keying what is counted per
// account, so that a person's handle and email are one account; it says
// nothing to the caller of a sign-in form.
func (s *Service) HandleFor(ctx context.Context, login string) string {
	u, err := s.byLogin(ctx, login)
	if err != nil || u == nil {
		return ""
	}
	return u.Handle
}

// byLogin finds a person by handle, or by email when login holds an "@",
// which no handle does. Nil when there is none.
func (s *Service) byLogin(ctx context.Context, login string) (*User, error) {
	login = normalizeEmail(login)
	if login == "" || len(login) > maxLoginLen {
		return nil, nil
	}
	var (
		u   *User
		err error
	)
	if strings.Contains(login, "@") {
		u, err = s.store.GetUserByEmail(ctx, login)
	} else {
		u, err = s.store.GetUser(ctx, login)
	}
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// maxLoginLen is the longest email address (RFC 5321).
const maxLoginLen = 254

// ChangePassword replaces handle's password, given the current one, and
// ends their browser sessions except keep, the one the change was made in.
// Keys and OAuth grants stay. ErrBadPassword for a wrong current password,
// ErrWeakPassword.
func (s *Service) ChangePassword(ctx context.Context, handle, current, next string, keep int64) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.change_password")
	defer func() { endSpan(span, err) }()
	if strings.Contains(handle, "@") {
		return ErrBadPassword
	}
	// Not CheckPassword: changing a password is no sign-in, and the hash
	// it would renew is about to be replaced.
	u, _, err := s.verify(ctx, handle, current)
	if err != nil {
		return err
	}
	return s.replacePassword(ctx, u.Handle, next, keep)
}

// ChangePasswordConfirmed is ChangePassword for a person the caller has
// confirmed another way, with one of their passkeys, in place of the current
// password. ErrNotFound, ErrDisabled, ErrWeakPassword, ErrLocalOff.
func (s *Service) ChangePasswordConfirmed(ctx context.Context, handle, next string, keep int64) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.change_password")
	defer func() { endSpan(span, err) }()
	if !s.opts.LocalAccounts {
		return ErrLocalOff
	}
	u, err := s.store.GetUser(ctx, handle)
	if err != nil {
		return err
	}
	if u.DisabledAt != nil {
		return ErrDisabled
	}
	return s.replacePassword(ctx, u.Handle, next, keep)
}

// replacePassword sets a confirmed person's new password, ends the links
// that would set another and their browser sessions except keep.
func (s *Service) replacePassword(ctx context.Context, handle, next string, keep int64) error {
	if err := checkPolicy(next, s.opts.MinPasswordLength); err != nil {
		return err
	}
	hash, err := s.hashing.hash(ctx, next)
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(ctx, handle, hash, s.now()); err != nil {
		return err
	}
	s.record(ctx, handle, audit.PasswordSet, handle, map[string]string{"handle": handle, "via": "change"})
	// A reset link made before the change would undo it.
	if err := s.store.RevokeUserLinks(ctx, handle, s.now()); err != nil {
		return fmt.Errorf("user: change password of %q: %w", handle, err)
	}
	if err := s.endSessions(ctx, handle, keep); err != nil {
		return fmt.Errorf("user: change password of %q: %w", handle, err)
	}
	return nil
}

// Accounts returns how every person signs in and when they last did,
// ordered by handle.
func (s *Service) Accounts(ctx context.Context) ([]Account, error) {
	return s.store.ListAccounts(ctx, s.now())
}

// Account is Accounts for one person. ErrNotFound.
func (s *Service) Account(ctx context.Context, handle string) (Account, error) {
	return s.store.GetAccount(ctx, handle, s.now())
}

// HashPassword hashes a password that guards something other than an
// account, such as a share link, with the parameters and the bound on hashes
// at once that accounts have. The caller checks its length. ErrBusy.
func (s *Service) HashPassword(ctx context.Context, password string) (string, error) {
	return s.hashing.hash(ctx, password)
}

// VerifyPassword reports whether password matches a hash from HashPassword.
// ErrBusy.
func (s *Service) VerifyPassword(ctx context.Context, hash, password string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	ok, _, err := s.hashing.verify(ctx, hash, password)
	return ok, err
}

// MinPasswordLength is the shortest password the instance accepts, for the
// pages that ask for one.
func (s *Service) MinPasswordLength() int { return s.opts.MinPasswordLength }
