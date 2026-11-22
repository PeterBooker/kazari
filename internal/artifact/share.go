package artifact

import (
	"context"
	"crypto/hmac"
	"errors"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/audit"
)

// Share link tokens are kzs_<prefix>_<secret>, lower-case unpadded base32
// like API keys: an 8-character prefix that names the link (40 bits) and a
// 52-character secret (256 bits).
const (
	LinkTokenPrefix = "kzs_"
	linkPrefixBytes = 5
	linkSecretBytes = 32
	linkPrefixLen   = 8
	linkSecretLen   = 52
)

var linkEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newLinkToken returns a fresh token and its prefix.
func newLinkToken() (token, prefix string) {
	var b [linkPrefixBytes + linkSecretBytes]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand panics on failure)
	prefix = linkEncoding.EncodeToString(b[:linkPrefixBytes])
	return LinkTokenPrefix + prefix + "_" + linkEncoding.EncodeToString(b[linkPrefixBytes:]), prefix
}

// IsLinkToken reports whether s has the exact shape of a share link token.
func IsLinkToken(s string) bool {
	rest, ok := strings.CutPrefix(s, LinkTokenPrefix)
	if !ok {
		return false
	}
	prefix, secret, ok := strings.Cut(rest, "_")
	return ok && len(prefix) == linkPrefixLen && len(secret) == linkSecretLen && isBase32(prefix) && isBase32(secret)
}

func isBase32(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// IsLinkCookie reports whether s has the shape of the view cookie a share
// link sets: its token, followed for a link with a password by a dot and
// the proof that the password was given.
func IsLinkCookie(s string) bool {
	token, _, _ := strings.Cut(s, ".")
	return IsLinkToken(token)
}

// Share-link passwords are ShareLinkPasswordMin to ShareLinkPasswordMax
// characters.
const (
	ShareLinkPasswordMin = 12
	ShareLinkPasswordMax = 256
)

// ErrLinkPassword matches, with errors.Is, a refusal of a share link's
// password, so a form can put it beside the password; the error's code is
// still invalid_argument.
var ErrLinkPassword = errors.New("artifact: share link password refused")

type linkPasswordError struct{ error }

func (e linkPasswordError) Is(target error) bool { return target == ErrLinkPassword }
func (e linkPasswordError) Unwrap() error        { return e.error }

// CheckLinkPassword refuses a share link's password that is not
// ShareLinkPasswordMin to ShareLinkPasswordMax characters of UTF-8.
func CheckLinkPassword(password string) error {
	n := utf8.RuneCountInString(password)
	if !utf8.ValidString(password) || n < ShareLinkPasswordMin || n > ShareLinkPasswordMax {
		return linkPasswordError{Errorf(ErrInvalidArgument, "a share link's password is %d to %d characters",
			ShareLinkPasswordMin, ShareLinkPasswordMax)}
	}
	return nil
}

// linkProof is what a correct password adds to a link's cookie: an HMAC of
// the token keyed with the stored password hash, which the URL does not
// carry.
func linkProof(passwordHash, token string) string {
	m := hmac.New(sha256.New, []byte(passwordHash))
	m.Write([]byte(token))
	return linkEncoding.EncodeToString(m.Sum(nil))
}

// hashLinkToken is the stored form of a token: hex SHA-256.
func hashLinkToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// shareable loads slug for a share-link change by actor: the artifact must
// be live or unpublished (not deleted or purged), readable by actor and
// changeable under the write policy, the same rules as a visibility change.
func (s *Service) shareable(ctx context.Context, actor Actor, slug string) (*Artifact, error) {
	if actor.Handle == "" {
		return nil, Errorf(ErrForbidden, "an authenticated actor is required")
	}
	if err := checkSlug(actor, slug); err != nil {
		return nil, err
	}
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	involved, err := s.involved(ctx, actor, slug)
	if err != nil {
		return nil, err
	}
	return a, s.checkWritable(actor, a, involved, false)
}

// CreateShareLink creates a link that opens slug without signing in until
// ttl has passed (ttl <= 0 means DefaultShareLinkTTL, capped at
// Options.MaxShareLinkTTL; a longer ttl is ErrInvalidArgument). label is a
// note such as who the link is for. A password, when not empty, must be
// given before the link opens anything. It returns the link and its token,
// which is never available again.
func (s *Service) CreateShareLink(ctx context.Context, actor Actor, slug, label string, ttl time.Duration, password string) (_ *ShareLink, _ string, err error) {
	ctx, span := s.span(ctx, "artifact.share_link.create", slug)
	defer func() {
		s.countOutcome(ctx, s.m.shareLinks, err, shareOperationKey.String("create"))
		endSpan(span, err)
	}()
	if err := checkSlug(actor, slug); err != nil {
		return nil, "", err
	}
	if ttl > s.opts.MaxShareLinkTTL {
		return nil, "", Errorf(ErrInvalidArgument, "a share link may live at most %s", formatTTL(s.opts.MaxShareLinkTTL))
	}
	if ttl <= 0 {
		ttl = min(DefaultShareLinkTTL, s.opts.MaxShareLinkTTL)
	}
	if err := validateText("label", label, LabelMaxBytes, false); err != nil {
		return nil, "", err
	}
	if password != "" {
		if err := CheckLinkPassword(password); err != nil {
			return nil, "", err
		}
		if s.opts.Passwords == nil {
			return nil, "", linkPasswordError{Errorf(ErrInvalidArgument, "this instance cannot keep share-link passwords")}
		}
	}
	a, err := s.shareable(ctx, actor, slug)
	if err != nil {
		return nil, "", err
	}
	// The link would open nothing: an unpublished page is served to nobody.
	if a.UnpublishedAt != nil {
		return nil, "", Errorf(ErrInvalidArgument,
			"%s is unpublished, so a share link would open nothing; set its visibility to members first", slug)
	}
	var passwordHash string
	if password != "" {
		if passwordHash, err = s.opts.Passwords.HashPassword(ctx, password); err != nil {
			return nil, "", err
		}
	}
	token, prefix := newLinkToken()
	now := s.now()
	l := &ShareLink{Prefix: prefix, Label: label, CreatedBy: actor.Handle, CreatedAt: now, ExpiresAt: now.Add(ttl),
		HasPassword: password != ""}
	if err := s.store.CreateShareLink(ctx, slug, l, hashLinkToken(token), passwordHash); err != nil {
		return nil, "", err
	}
	details := map[string]string{"prefix": prefix, "expires_at": l.ExpiresAt.Format(time.RFC3339)}
	if label != "" {
		details["label"] = label
	}
	if l.HasPassword {
		details["password"] = "true"
	}
	s.record(ctx, actor, audit.ShareLinkCreate, slug, details)
	return l, token, nil
}

// ListShareLinks returns slug's links, newest first, to an actor who may
// change its sharing.
func (s *Service) ListShareLinks(ctx context.Context, actor Actor, slug string) ([]ShareLink, error) {
	if _, err := s.shareable(ctx, actor, slug); err != nil {
		return nil, err
	}
	return s.store.ListShareLinks(ctx, slug)
}

// RevokeShareLink ends the link with prefix at once; browsers that opened
// it lose access on their next request. Revoking twice is not an error.
func (s *Service) RevokeShareLink(ctx context.Context, actor Actor, slug, prefix string) (err error) {
	ctx, span := s.span(ctx, "artifact.share_link.revoke", slug)
	defer func() {
		s.countOutcome(ctx, s.m.shareLinks, err, shareOperationKey.String("revoke"))
		endSpan(span, err)
	}()
	if _, err := s.shareable(ctx, actor, slug); err != nil {
		return err
	}
	if err := s.store.RevokeShareLink(ctx, slug, prefix, s.now()); err != nil {
		return err
	}
	s.record(ctx, actor, audit.ShareLinkRevoke, slug, map[string]string{"prefix": prefix})
	return nil
}

// RevokeUserShareLinks ends every link handle created, on any artifact, so a
// disabled user keeps no way in. It is wired to user.Options.OnDisable, so
// enabling the user does not restore them.
func (s *Service) RevokeUserShareLinks(ctx context.Context, actor Actor, handle string) (err error) {
	ctx, span := s.span(ctx, "artifact.share_link.revoke_user", "")
	defer func() { endSpan(span, err) }()
	revoked, err := s.store.RevokeUserShareLinks(ctx, handle, s.now())
	if err != nil {
		s.countOutcome(ctx, s.m.shareLinks, err, shareOperationKey.String("revoke"))
		return err
	}
	for _, l := range revoked {
		s.countOutcome(ctx, s.m.shareLinks, nil, shareOperationKey.String("revoke"))
		s.record(ctx, actor, audit.ShareLinkRevoke, l.Slug, map[string]string{"prefix": l.Prefix, "created_by": handle})
	}
	return nil
}

// activeLink returns slug's active link for token and its password hash;
// anything else is ErrLinkNotFound.
func (s *Service) activeLink(ctx context.Context, slug, token string) (*ShareLink, string, error) {
	if !IsLinkToken(token) {
		return nil, "", ErrLinkNotFound
	}
	l, passwordHash, err := s.store.ShareLink(ctx, slug, hashLinkToken(token))
	if err != nil {
		return nil, "", err
	}
	if !l.Active(s.now()) {
		return nil, "", ErrLinkNotFound
	}
	return l, passwordHash, nil
}

// CheckShareLink returns slug's active link for the view cookie a browser
// was given when it opened the link, for the serving path: the token alone,
// or for a link with a password the token and the proof that the password
// was given. Anything else (a malformed, unknown, revoked or expired token,
// a missing or wrong proof, or another artifact's link) is ErrLinkNotFound.
func (s *Service) CheckShareLink(ctx context.Context, slug, cookie string) (*ShareLink, error) {
	token, proof, _ := strings.Cut(cookie, ".")
	l, passwordHash, err := s.activeLink(ctx, slug, token)
	if err != nil {
		return nil, err
	}
	want := ""
	if passwordHash != "" {
		want = linkProof(passwordHash, token)
	}
	if !hmac.Equal([]byte(proof), []byte(want)) {
		return nil, ErrLinkNotFound
	}
	return l, nil
}

// OpenShareLink is a browser opening the link with its token: it returns
// the link and the view cookie that opens slug, and counts the use. A link
// with a password returns no cookie and counts nothing: the browser asks
// for the password and calls UnlockShareLink.
func (s *Service) OpenShareLink(ctx context.Context, slug, token string) (_ *ShareLink, cookie string, err error) {
	ctx, span := s.span(ctx, "artifact.share_link.open", slug)
	defer func() { s.countOutcome(ctx, s.m.shareLinks, err, shareOperationKey.String("open")); endSpan(span, err) }()
	l, passwordHash, err := s.activeLink(ctx, slug, token)
	if err != nil {
		return nil, "", err
	}
	if passwordHash != "" {
		return l, "", nil
	}
	return s.used(ctx, slug, l, token)
}

// UnlockShareLink opens a link with its password: a wrong password is
// ErrForbidden, the right one returns the link and its view cookie and
// counts the use. Slowing guesses down is the caller's part.
func (s *Service) UnlockShareLink(ctx context.Context, slug, token, password string) (_ *ShareLink, cookie string, err error) {
	ctx, span := s.span(ctx, "artifact.share_link.unlock", slug)
	defer func() {
		s.countOutcome(ctx, s.m.shareLinks, err, shareOperationKey.String("unlock"))
		endSpan(span, err)
	}()
	l, passwordHash, err := s.activeLink(ctx, slug, token)
	if err != nil {
		return nil, "", err
	}
	if passwordHash == "" || s.opts.Passwords == nil {
		return nil, "", ErrLinkNotFound
	}
	ok, err := s.opts.Passwords.VerifyPassword(ctx, passwordHash, password)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", Errorf(ErrForbidden, "the password is not right")
	}
	return s.used(ctx, slug, l, token+"."+linkProof(passwordHash, token))
}

// used counts one opening of l and returns it with cookie.
func (s *Service) used(ctx context.Context, slug string, l *ShareLink, cookie string) (*ShareLink, string, error) {
	now := s.now()
	if err := s.store.UseShareLink(ctx, slug, l.Prefix, now); err != nil {
		return nil, "", err
	}
	l.Uses++
	l.LastUsedAt = &now
	return l, cookie, nil
}

// formatTTL prints a whole number of days as "90d", anything else as a
// Go duration.
func formatTTL(d time.Duration) string {
	const day = 24 * time.Hour
	if d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	return d.String()
}
