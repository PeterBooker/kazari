package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/upload"
)

// UploadStore implements upload.Store.
type UploadStore struct {
	db *DB
}

var _ upload.Store = (*UploadStore)(nil)

// NewUploadStore returns an upload store backed by db.
func NewUploadStore(db *DB) *UploadStore {
	return &UploadStore{db: db}
}

// Create needs an existing owner. The count runs in the insert's immediate
// transaction, so concurrent creates cannot both take the last slot.
func (s *UploadStore) Create(ctx context.Context, u *upload.Upload, maxOpen int) (err error) {
	ctx, end := s.db.op(ctx, "UploadStore.Create")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var ownerID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM user WHERE handle = ?`, u.Owner).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unknown owner %q", u.Owner)
		}
		if err != nil {
			return err
		}
		if maxOpen > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM upload WHERE owner_id = ?`, ownerID).Scan(&n); err != nil {
				return err
			}
			if n >= maxOpen {
				return artifact.Errorf(artifact.ErrTooManyUploads, "%d uploads already open (limit %d)", n, maxOpen)
			}
		}
		a := u.Archive
		if a == nil {
			a = &upload.Archive{}
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO upload (id, owner_id, bytes, created_at, expires_at,
			   archive_token_hash, archive_sha256, archive_bytes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, ownerID, u.Bytes, toNanos(u.CreatedAt), toNanos(u.ExpiresAt), a.TokenHash, a.SHA256, a.Bytes)
		return err
	})
	if artifact.CodeOf(err) != "" {
		return err
	}
	if err != nil {
		return fmt.Errorf("sqlite: create upload %s: %w", u.ID, err)
	}
	return nil
}

// Get sorts files by path.
func (s *UploadStore) Get(ctx context.Context, id string) (_ *upload.Upload, err error) {
	ctx, end := s.db.op(ctx, "UploadStore.Get")
	defer func() { end(err) }()
	var u *upload.Upload
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+uploadColumns+`
			 FROM upload u JOIN user usr ON usr.id = u.owner_id
			 WHERE u.id = ?`, id)
		var err error
		u, err = scanUpload(row.Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return artifact.ErrUploadNotFound
		}
		if err != nil {
			return err
		}
		u.Files, err = uploadFiles(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: get upload %s: %w", id, err)
	}
	return u, nil
}

// UpsertFile returns artifact.ErrUploadNotFound for an unknown id.
func (s *UploadStore) UpsertFile(ctx context.Context, id string, f upload.StagedFile, totalBytes int64) (err error) {
	ctx, end := s.db.op(ctx, "UploadStore.UpsertFile")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE upload SET bytes = ? WHERE id = ?`, totalBytes, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return artifact.ErrUploadNotFound
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO upload_file (upload_id, path, staging_index, bytes, content_type)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (upload_id, path) DO UPDATE SET
			   staging_index = excluded.staging_index,
			   bytes = excluded.bytes,
			   content_type = excluded.content_type`,
			id, f.Path, f.StagingIndex, f.Bytes, f.ContentType)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: upsert upload file %s %q: %w", id, f.Path, err)
	}
	return nil
}

// ReceiveArchive spends the archive in the same statement that checks it is
// unspent, so two bodies racing for one token cannot both be received.
func (s *UploadStore) ReceiveArchive(ctx context.Context, id string, files []upload.StagedFile, totalBytes int64, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UploadStore.ReceiveArchive")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE upload SET bytes = ?, archive_received_at = ?
			 WHERE id = ? AND archive_token_hash != '' AND archive_received_at IS NULL`,
			totalBytes, toNanos(at), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return artifact.ErrUploadNotFound
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO upload_file (upload_id, path, staging_index, bytes, content_type) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, f := range files {
			if _, err := stmt.ExecContext(ctx, id, f.Path, f.StagingIndex, f.Bytes, f.ContentType); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: receive archive %s: %w", id, err)
	}
	return nil
}

// CountArchiveAttempt returns artifact.ErrUploadNotFound for an unknown id.
func (s *UploadStore) CountArchiveAttempt(ctx context.Context, id string) (_ int, err error) {
	ctx, end := s.db.op(ctx, "UploadStore.CountArchiveAttempt")
	defer func() { end(err) }()
	var n int
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE upload SET archive_attempts = archive_attempts + 1 WHERE id = ?`, id)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return artifact.ErrUploadNotFound
		}
		return tx.QueryRowContext(ctx, `SELECT archive_attempts FROM upload WHERE id = ?`, id).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: count archive attempt %s: %w", id, err)
	}
	return n, nil
}

// Delete returns artifact.ErrUploadNotFound for an unknown id.
func (s *UploadStore) Delete(ctx context.Context, id string) (err error) {
	ctx, end := s.db.op(ctx, "UploadStore.Delete")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM upload_file WHERE upload_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM upload WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return artifact.ErrUploadNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: delete upload %s: %w", id, err)
	}
	return nil
}

// ListExpired returns the oldest expiry first, each upload with its files.
func (s *UploadStore) ListExpired(ctx context.Context, now time.Time) (_ []upload.Upload, err error) {
	ctx, end := s.db.op(ctx, "UploadStore.ListExpired")
	defer func() { end(err) }()
	var out []upload.Upload
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT `+uploadColumns+`
			 FROM upload u JOIN user usr ON usr.id = u.owner_id
			 WHERE u.expires_at <= ?
			 ORDER BY u.expires_at, u.id`, toNanos(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUpload(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, *u)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for i := range out {
			if out[i].Files, err = uploadFiles(ctx, tx, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list expired uploads: %w", err)
	}
	return out, nil
}

// CountOpen counts the uploads owned by the handle, expired or not.
func (s *UploadStore) CountOpen(ctx context.Context, owner string) (_ int, err error) {
	ctx, end := s.db.op(ctx, "UploadStore.CountOpen")
	defer func() { end(err) }()
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM upload u JOIN user usr ON usr.id = u.owner_id
		 WHERE usr.handle = ?`, owner).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count uploads of %q: %w", owner, err)
	}
	return n, nil
}

const uploadColumns = `u.id, usr.handle, u.bytes, u.created_at, u.expires_at,
	u.archive_token_hash, u.archive_sha256, u.archive_bytes, u.archive_attempts, u.archive_received_at`

func scanUpload(scan func(dest ...any) error) (*upload.Upload, error) {
	var (
		u                  upload.Upload
		a                  upload.Archive
		created, expiresAt int64
		received           sql.NullInt64
	)
	if err := scan(&u.ID, &u.Owner, &u.Bytes, &created, &expiresAt,
		&a.TokenHash, &a.SHA256, &a.Bytes, &a.Attempts, &received); err != nil {
		return nil, err
	}
	u.CreatedAt = fromNanos(created)
	u.ExpiresAt = fromNanos(expiresAt)
	if a.TokenHash != "" {
		if received.Valid {
			a.ReceivedAt = fromNanos(received.Int64)
		}
		u.Archive = &a
	}
	return &u, nil
}

func uploadFiles(ctx context.Context, tx *sql.Tx, id string) ([]upload.StagedFile, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, staging_index, bytes, content_type FROM upload_file
		 WHERE upload_id = ? ORDER BY path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []upload.StagedFile
	for rows.Next() {
		var f upload.StagedFile
		if err := rows.Scan(&f.Path, &f.StagingIndex, &f.Bytes, &f.ContentType); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *UploadStore) CountAll(ctx context.Context) (n int64, err error) {
	ctx, end := s.db.op(ctx, "UploadStore.CountAll")
	defer func() { end(err) }()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count uploads: %w", err)
	}
	return n, nil
}
