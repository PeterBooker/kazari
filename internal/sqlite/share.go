package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// CreateShareLink implements artifact.Store.
func (s *ArtifactStore) CreateShareLink(ctx context.Context, slug string, l *artifact.ShareLink, hash string) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		aid, err := lookupArtifactID(ctx, tx, slug)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO share_link (artifact_id, prefix, token_hash, label, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			aid, l.Prefix, hash, l.Label, l.CreatedBy, toNanos(l.CreatedAt), toNanos(l.ExpiresAt))
		return err
	})
	return artifactErrOrNil("create share link", err)
}

const shareLinkSelect = `SELECT l.prefix, l.label, l.created_by, l.created_at, l.expires_at, l.revoked_at,
	l.last_used_at, l.uses
FROM share_link l JOIN artifact a ON a.id = l.artifact_id`

func scanShareLink(row artifactRowScanner) (*artifact.ShareLink, error) {
	var (
		l                artifact.ShareLink
		created, expires int64
		revoked, used    sql.NullInt64
	)
	if err := row.Scan(&l.Prefix, &l.Label, &l.CreatedBy, &created, &expires, &revoked, &used, &l.Uses); err != nil {
		return nil, err
	}
	l.CreatedAt = fromNanos(created)
	l.ExpiresAt = fromNanos(expires)
	l.RevokedAt = ptrTime(revoked)
	l.LastUsedAt = ptrTime(used)
	return &l, nil
}

// ListShareLinks implements artifact.Store.
func (s *ArtifactStore) ListShareLinks(ctx context.Context, slug string) ([]artifact.ShareLink, error) {
	rows, err := s.db.QueryContext(ctx, shareLinkSelect+` WHERE a.slug = ? ORDER BY l.created_at DESC, l.id DESC`, slug)
	if err != nil {
		return nil, artifactErr("list share links", err)
	}
	defer rows.Close()
	out := []artifact.ShareLink{}
	for rows.Next() {
		l, err := scanShareLink(rows)
		if err != nil {
			return nil, artifactErr("list share links", err)
		}
		out = append(out, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, artifactErr("list share links", err)
	}
	return out, nil
}

// RevokeShareLink implements artifact.Store.
func (s *ArtifactStore) RevokeShareLink(ctx context.Context, slug, prefix string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE share_link SET revoked_at = COALESCE(revoked_at, ?)
		 WHERE prefix = ? AND artifact_id = (SELECT id FROM artifact WHERE slug = ?)`, toNanos(at), prefix, slug)
	if err != nil {
		return artifactErr("revoke share link", err)
	}
	return shareLinkTouched(res, "revoke share link")
}

// ShareLink implements artifact.Store.
func (s *ArtifactStore) ShareLink(ctx context.Context, slug, hash string) (*artifact.ShareLink, error) {
	l, err := scanShareLink(s.db.QueryRowContext(ctx, shareLinkSelect+` WHERE l.token_hash = ? AND a.slug = ?`, hash, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrLinkNotFound
	}
	if err != nil {
		return nil, artifactErr("share link", err)
	}
	return l, nil
}

// UseShareLink implements artifact.Store.
func (s *ArtifactStore) UseShareLink(ctx context.Context, slug, prefix string, at time.Time) error {
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
