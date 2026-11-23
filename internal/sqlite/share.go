package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// CreateShareLink implements artifact.Store.
func (s *ArtifactStore) CreateShareLink(ctx context.Context, slug string, l *artifact.ShareLink, hash, passwordHash string) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.CreateShareLink")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		aid, err := lookupArtifactID(ctx, tx, slug)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO share_link (artifact_id, prefix, token_hash, password_hash, label, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			aid, l.Prefix, hash, passwordHash, l.Label, l.CreatedBy, toNanos(l.CreatedAt), toNanos(l.ExpiresAt))
		return err
	})
	return artifactErr("create share link", err)
}

const shareLinkSelect = `SELECT l.prefix, l.label, l.created_by, l.created_at, l.expires_at, l.revoked_at,
	l.last_used_at, l.uses, l.password_hash
FROM share_link l JOIN artifact a ON a.id = l.artifact_id`

// scanShareLink returns the link and its password hash, "" for none.
func scanShareLink(row rowScanner) (*artifact.ShareLink, string, error) {
	var (
		l                artifact.ShareLink
		created, expires int64
		revoked, used    sql.NullInt64
		passwordHash     string
	)
	if err := row.Scan(&l.Prefix, &l.Label, &l.CreatedBy, &created, &expires, &revoked, &used, &l.Uses, &passwordHash); err != nil {
		return nil, "", err
	}
	l.CreatedAt = fromNanos(created)
	l.ExpiresAt = fromNanos(expires)
	l.RevokedAt = ptrTime(revoked)
	l.LastUsedAt = ptrTime(used)
	l.HasPassword = passwordHash != ""
	return &l, passwordHash, nil
}

// ListShareLinks implements artifact.Store.
func (s *ArtifactStore) ListShareLinks(ctx context.Context, slug string) (_ []artifact.ShareLink, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ListShareLinks")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx, shareLinkSelect+` WHERE a.slug = ? ORDER BY l.created_at DESC, l.id DESC`, slug)
	out, err := collect(rows, err, func(row rowScanner) (artifact.ShareLink, error) {
		l, _, err := scanShareLink(row)
		if err != nil {
			return artifact.ShareLink{}, err
		}
		return *l, nil
	})
	if err != nil {
		return nil, artifactErr("list share links", err)
	}
	if out == nil {
		out = []artifact.ShareLink{}
	}
	return out, nil
}

// RevokeShareLink implements artifact.Store.
func (s *ArtifactStore) RevokeShareLink(ctx context.Context, slug, prefix string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.RevokeShareLink")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx,
		`UPDATE share_link SET revoked_at = COALESCE(revoked_at, ?)
		 WHERE prefix = ? AND artifact_id = (SELECT id FROM artifact WHERE slug = ?)`, toNanos(at), prefix, slug)
	if err != nil {
		return artifactErr("revoke share link", err)
	}
	return shareLinkTouched(res, "revoke share link")
}

// RevokeUserShareLinks implements artifact.Store.
func (s *ArtifactStore) RevokeUserShareLinks(ctx context.Context, handle string, at time.Time) (_ []artifact.RevokedShareLink, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.RevokeUserShareLinks")
	defer func() { end(err) }()
	var out []artifact.RevokedShareLink
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`UPDATE share_link SET revoked_at = ? WHERE created_by = ? AND revoked_at IS NULL
			 RETURNING (SELECT slug FROM artifact WHERE id = artifact_id), prefix`, toNanos(at), handle)
		out, err = collect(rows, err, func(row rowScanner) (artifact.RevokedShareLink, error) {
			var l artifact.RevokedShareLink
			err := row.Scan(&l.Slug, &l.Prefix)
			return l, err
		})
		return err
	})
	if err != nil {
		return nil, artifactErr("revoke user share links", err)
	}
	return out, nil
}

// ShareLink implements artifact.Store.
func (s *ArtifactStore) ShareLink(ctx context.Context, slug, hash string) (_ *artifact.ShareLink, _ string, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ShareLink")
	defer func() { end(err) }()
	l, passwordHash, err := scanShareLink(s.db.QueryRowContext(ctx, shareLinkSelect+` WHERE l.token_hash = ? AND a.slug = ?`, hash, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", artifact.ErrLinkNotFound
	}
	if err != nil {
		return nil, "", artifactErr("share link", err)
	}
	return l, passwordHash, nil
}

// UseShareLink implements artifact.Store.
func (s *ArtifactStore) UseShareLink(ctx context.Context, slug, prefix string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.UseShareLink")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx,
		`UPDATE share_link SET uses = uses + 1, last_used_at = ?
		 WHERE prefix = ? AND artifact_id = (SELECT id FROM artifact WHERE slug = ?)`, toNanos(at), prefix, slug)
	if err != nil {
		return artifactErr("use share link", err)
	}
	return shareLinkTouched(res, "use share link")
}

// shareLinkTouched maps an update that matched no row to ErrLinkNotFound.
func shareLinkTouched(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return artifactErr(op, err)
	}
	if n == 0 {
		return artifact.ErrLinkNotFound
	}
	return nil
}
