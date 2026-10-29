package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

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

// hashLinkToken is the stored form of a token: hex SHA-256.
func hashLinkToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// shareable loads slug for a share-link change by actor: the artifact must
// be live or unpublished (not deleted or purged), readable by actor and
// changeable under the write policy, the same rules as a visibility change.
func (s *Service) shareable(ctx context.Context, actor Actor, slug string) error {
	if actor.Handle == "" {
		return Errorf(ErrForbidden, "an authenticated actor is required")
	}
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return err
	}
	involved, err := s.involved(ctx, actor, slug)
	if err != nil {
		return err
	}
	return s.checkWritable(actor, a, involved, false)
}

// CreateShareLink creates a link that opens slug without signing in until
// ttl has passed (ttl <= 0 means DefaultShareLinkTTL, capped at
// Options.MaxShareLinkTTL; a longer ttl is ErrInvalidArgument). label is a
// note such as who the link is for. It returns the link and its token,
// which is never available again.
func (s *Service) CreateShareLink(ctx context.Context, actor Actor, slug, label string, ttl time.Duration) (*ShareLink, string, error) {
	if ttl > s.opts.MaxShareLinkTTL {
		return nil, "", Errorf(ErrInvalidArgument, "a share link may live at most %s", formatTTL(s.opts.MaxShareLinkTTL))
	}
	if ttl <= 0 {
		ttl = min(DefaultShareLinkTTL, s.opts.MaxShareLinkTTL)
	}
	if err := validateText("label", label, LabelMaxBytes, false); err != nil {
		return nil, "", err
	}
	if err := s.shareable(ctx, actor, slug); err != nil {
		return nil, "", err
	}
	token, prefix := newLinkToken()
	now := s.now()
	l := &ShareLink{Prefix: prefix, Label: label, CreatedBy: actor.Handle, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := s.store.CreateShareLink(ctx, slug, l, hashLinkToken(token)); err != nil {
		return nil, "", err
	}
	s.record(ctx, actor, audit.ShareLinkCreate, slug, map[string]string{
		"prefix": prefix, "expires_at": l.ExpiresAt.Format(time.RFC3339),
	})
	return l, token, nil
}

// ListShareLinks returns slug's links, newest first, to an actor who may
// change its sharing.
func (s *Service) ListShareLinks(ctx context.Context, actor Actor, slug string) ([]ShareLink, error) {
	if err := s.shareable(ctx, actor, slug); err != nil {
		return nil, err
	}
	return s.store.ListShareLinks(ctx, slug)
}

// RevokeShareLink ends the link with prefix at once; browsers that opened
// it lose access on their next request. Revoking twice is not an error.
func (s *Service) RevokeShareLink(ctx context.Context, actor Actor, slug, prefix string) error {
	if err := s.shareable(ctx, actor, slug); err != nil {
		return err
	}
	if err := s.store.RevokeShareLink(ctx, slug, prefix, s.now()); err != nil {
		return err
	}
	s.record(ctx, actor, audit.ShareLinkRevoke, slug, map[string]string{"prefix": prefix})
	return nil
}

// CheckShareLink returns slug's active link for token, for the serving
// path. Anything else (a malformed, unknown, revoked or expired token, or
// another artifact's) is ErrLinkNotFound.
func (s *Service) CheckShareLink(ctx context.Context, slug, token string) (*ShareLink, error) {
	if !IsLinkToken(token) {
		return nil, ErrLinkNotFound
	}
	l, err := s.store.ShareLink(ctx, slug, hashLinkToken(token))
	if err != nil {
		return nil, err
	}
	if !l.Active(s.now()) {
		return nil, ErrLinkNotFound
	}
	return l, nil
}

// OpenShareLink is CheckShareLink for a browser opening the link: it also
// counts the use.
func (s *Service) OpenShareLink(ctx context.Context, slug, token string) (*ShareLink, error) {
	l, err := s.CheckShareLink(ctx, slug, token)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.store.UseShareLink(ctx, slug, l.Prefix, now); err != nil {
		return nil, err
	}
	l.Uses++
	l.LastUsedAt = &now
	return l, nil
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
