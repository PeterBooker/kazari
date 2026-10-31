package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// ArtifactStore implements artifact.Store.
type ArtifactStore struct {
	db *DB
}

var _ artifact.Store = (*ArtifactStore)(nil)

// NewArtifactStore returns an artifact.Store backed by db.
func NewArtifactStore(db *DB) *ArtifactStore {
	return &ArtifactStore{db: db}
}

// Documented on artifact.ListQuery.
const (
	artifactListDefault = 50
	artifactListMax     = 200
)

// The derived numbers are subqueries over version so they are always current.
const artifactSelect = `SELECT a.id, a.slug, a.name, a.description, u.handle, a.visibility,
	COALESCE((SELECT pv.number FROM version pv WHERE pv.id = a.published_version_id), 0),
	COALESCE((SELECT MAX(lv.number) FROM version lv WHERE lv.artifact_id = a.id), 0),
	COALESCE((SELECT SUM(sv.size_bytes) FROM version sv WHERE sv.artifact_id = a.id), 0),
	a.metadata, a.stale_at, a.expires_at, a.unpublished_at, a.deleted_at, a.purged_at,
	a.created_at, a.updated_at
FROM artifact a JOIN user u ON u.id = a.owner_id`

type artifactRowScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(row artifactRowScanner) (int64, *artifact.Artifact, error) {
	var (
		id                                int64
		a                                 artifact.Artifact
		vis, meta                         string
		stale, expires, unpub, del, purge sql.NullInt64
		created, updated                  int64
	)
	err := row.Scan(&id, &a.Slug, &a.Name, &a.Description, &a.OwnerHandle, &vis,
		&a.PublishedVersion, &a.LatestVersion, &a.SizeBytes,
		&meta, &stale, &expires, &unpub, &del, &purge, &created, &updated)
	if err != nil {
		return 0, nil, err
	}
	a.Visibility = artifact.Visibility(vis)
	if a.Metadata, err = decodeArtifactMetadata(meta); err != nil {
		return 0, nil, err
	}
	a.StaleAt = ptrTime(stale)
	a.ExpiresAt = ptrTime(expires)
	a.UnpublishedAt = ptrTime(unpub)
	a.DeletedAt = ptrTime(del)
	a.PurgedAt = ptrTime(purge)
	a.CreatedAt = fromNanos(created)
	a.UpdatedAt = fromNanos(updated)
	return id, &a, nil
}

func encodeArtifactMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("sqlite: encode metadata: %w", err)
	}
	return string(b), nil
}

// decodeArtifactMetadata always returns a non-nil map.
func decodeArtifactMetadata(s string) (map[string]string, error) {
	m := map[string]string{}
	if s == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("sqlite: decode metadata: %w", err)
	}
	return m, nil
}

func artifactIsUnique(err error) bool {
	return errors.Is(err, sqlite3.CONSTRAINT_UNIQUE) || errors.Is(err, sqlite3.CONSTRAINT_PRIMARYKEY)
}

func artifactUserID(ctx context.Context, tx *sql.Tx, handle string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM user WHERE handle = ?`, handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, artifact.Errorf(artifact.ErrInvalidArgument, "unknown user %q", handle)
	}
	return id, err
}

func lookupArtifactID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, slug string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM artifact WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, artifact.ErrNotFound
	}
	return id, err
}

func insertArtifactVersion(ctx context.Context, tx *sql.Tx, aid int64, number int, v *artifact.Version) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO version (artifact_id, number, label, created_by, created_at, size_bytes)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		aid, number, v.Label, v.CreatedBy, toNanos(v.CreatedAt), v.SizeBytes)
	if err != nil {
		if artifactIsUnique(err) {
			return 0, artifact.ErrVersionConflict
		}
		return 0, err
	}
	vid, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	fileStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO version_file (version_id, path, blob_hash, size, content_type) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer fileStmt.Close()
	blobStmt, err := tx.PrepareContext(ctx, artifactUpsertBlob)
	if err != nil {
		return 0, err
	}
	defer blobStmt.Close()
	for _, f := range v.Files {
		if _, err := fileStmt.ExecContext(ctx, vid, f.Path, f.Hash, f.Size, f.ContentType); err != nil {
			if artifactIsUnique(err) {
				return 0, artifact.Errorf(artifact.ErrInvalidArgument, "duplicate path %q", f.Path)
			}
			return 0, err
		}
		if _, err := blobStmt.ExecContext(ctx, f.Hash, f.Size, toNanos(v.CreatedAt)); err != nil {
			return 0, err
		}
	}
	return vid, nil
}

const artifactUpsertBlob = `INSERT INTO blob (hash, size, created_at) VALUES (?, ?, ?)
	ON CONFLICT (hash) DO UPDATE SET created_at = MAX(blob.created_at, excluded.created_at)`

// markArtifactActor records nothing for an unknown handle (e.g. the scheduler).
func markArtifactActor(ctx context.Context, tx *sql.Tx, aid int64, handle string, created bool, at time.Time) error {
	c, u := 0, 1
	if created {
		c, u = 1, 0
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO artifact_actor (artifact_id, user_id, created, updated, last_at)
		 SELECT ?, id, ?, ?, ? FROM user WHERE handle = ?
		 ON CONFLICT (artifact_id, user_id) DO UPDATE SET
		   created = MAX(artifact_actor.created, excluded.created),
		   updated = MAX(artifact_actor.updated, excluded.updated),
		   last_at = MAX(artifact_actor.last_at, excluded.last_at)`,
		aid, c, u, toNanos(at), handle)
	return err
}

const artifactUsage = `SELECT COALESCE(SUM(v.size_bytes), 0) FROM version v
	JOIN artifact a ON a.id = v.artifact_id
	JOIN user u ON u.id = a.owner_id
	WHERE u.handle = ?`

// checkArtifactQuota runs inside the write transaction, which is immediate,
// so concurrent publishes are checked one after the other.
func checkArtifactQuota(ctx context.Context, tx *sql.Tx, owner string, size, quota int64) error {
	if quota <= 0 {
		return nil
	}
	var used int64
	if err := tx.QueryRowContext(ctx, artifactUsage, owner).Scan(&used); err != nil {
		return err
	}
	if used+size > quota {
		return artifact.Errorf(artifact.ErrQuotaExceeded, "the new version would exceed the owner's quota")
	}
	return nil
}

func (s *ArtifactStore) Create(ctx context.Context, a *artifact.Artifact, v *artifact.Version, quota int64) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Create")
	defer func() { end(err) }()
	meta, err := encodeArtifactMetadata(a.Metadata)
	if err != nil {
		return err
	}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		ownerID, err := artifactUserID(ctx, tx, a.OwnerHandle)
		if err != nil {
			return err
		}
		if err := checkArtifactQuota(ctx, tx, a.OwnerHandle, v.SizeBytes, quota); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO artifact (slug, name, description, owner_id, visibility, metadata,
			   stale_at, expires_at, unpublished_at, deleted_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Slug, a.Name, a.Description, ownerID, string(a.Visibility), meta,
			nullNanos(a.StaleAt), nullNanos(a.ExpiresAt), nullNanos(a.UnpublishedAt), nullNanos(a.DeletedAt),
			toNanos(a.CreatedAt), toNanos(a.UpdatedAt))
		if err != nil {
			if artifactIsUnique(err) {
				return artifact.Errorf(artifact.ErrSlugTaken, "slug %q is already taken", a.Slug)
			}
			return err
		}
		aid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		vid, err := insertArtifactVersion(ctx, tx, aid, 1, v)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE artifact SET published_version_id = ? WHERE id = ?`, vid, aid); err != nil {
			return err
		}
		if err := markArtifactActor(ctx, tx, aid, a.OwnerHandle, true, a.CreatedAt); err != nil {
			return err
		}
		v.ID, v.Number = vid, 1
		return nil
	})
	if err != nil {
		return artifactErr("create", err)
	}
	a.PublishedVersion, a.LatestVersion, a.SizeBytes = 1, 1, v.SizeBytes
	return nil
}

func artifactErr(op string, err error) error {
	if artifact.CodeOf(err) != "" {
		return err
	}
	return fmt.Errorf("sqlite: artifact %s: %w", op, err)
}

func (s *ArtifactStore) Get(ctx context.Context, slug string) (_ *artifact.Artifact, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Get")
	defer func() { end(err) }()
	_, a, err := scanArtifact(s.db.QueryRowContext(ctx, artifactSelect+` WHERE a.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, artifactErr("get", err)
	}
	return a, nil
}

func (s *ArtifactStore) GetVersion(ctx context.Context, slug string, number int) (_ *artifact.Version, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.GetVersion")
	defer func() { end(err) }()
	aid, err := lookupArtifactID(ctx, s.db, slug)
	if err != nil {
		return nil, artifactErr("get version", err)
	}
	var (
		v       artifact.Version
		created int64
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT id, number, label, created_by, created_at, size_bytes FROM version
		 WHERE artifact_id = ? AND number = ?`, aid, number).
		Scan(&v.ID, &v.Number, &v.Label, &v.CreatedBy, &created, &v.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrVersionNotFound
	}
	if err != nil {
		return nil, artifactErr("get version", err)
	}
	v.CreatedAt = fromNanos(created)
	files, err := s.files(ctx, v.ID)
	if err != nil {
		return nil, artifactErr("get version", err)
	}
	v.Files = files
	return &v, nil
}

func (s *ArtifactStore) files(ctx context.Context, versionID int64) ([]artifact.File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, blob_hash, size, content_type FROM version_file WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []artifact.File
	for rows.Next() {
		var f artifact.File
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &f.ContentType); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *ArtifactStore) ListVersions(ctx context.Context, slug string) (_ []artifact.Version, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ListVersions")
	defer func() { end(err) }()
	aid, err := lookupArtifactID(ctx, s.db, slug)
	if err != nil {
		return nil, artifactErr("list versions", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, number, label, created_by, created_at, size_bytes FROM version
		 WHERE artifact_id = ? ORDER BY number DESC`, aid)
	if err != nil {
		return nil, artifactErr("list versions", err)
	}
	defer rows.Close()
	var out []artifact.Version
	for rows.Next() {
		var (
			v       artifact.Version
			created int64
		)
		if err := rows.Scan(&v.ID, &v.Number, &v.Label, &v.CreatedBy, &created, &v.SizeBytes); err != nil {
			return nil, artifactErr("list versions", err)
		}
		v.CreatedAt = fromNanos(created)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, artifactErr("list versions", err)
	}
	return out, nil
}

func readArtifactTx(ctx context.Context, tx *sql.Tx, slug string) (int64, *artifact.Artifact, error) {
	aid, a, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE a.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, artifact.ErrNotFound
	}
	return aid, a, err
}

func (s *ArtifactStore) AddVersion(ctx context.Context, slug string, v *artifact.Version, expected int, quota int64,
	fn func(a *artifact.Artifact) error) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.AddVersion")
	defer func() { end(err) }()
	var number int
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		aid, a, err := readArtifactTx(ctx, tx, slug)
		if err != nil {
			return err
		}
		// fn cannot change the owner or the versions; take them first.
		latest, owner := a.LatestVersion, a.OwnerHandle
		if err := fn(a); err != nil {
			return err
		}
		if expected != 0 && latest != expected {
			return artifact.Errorf(artifact.ErrVersionConflict, "latest version is %d, expected %d", latest, expected)
		}
		if err := checkArtifactQuota(ctx, tx, owner, v.SizeBytes, quota); err != nil {
			return err
		}
		meta, err := encodeArtifactMetadata(a.Metadata)
		if err != nil {
			return err
		}
		number = latest + 1
		vid, err := insertArtifactVersion(ctx, tx, aid, number, v)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE artifact SET name = ?, description = ?, visibility = ?, metadata = ?,
			   stale_at = ?, expires_at = ?, unpublished_at = ?, deleted_at = ?, updated_at = ?,
			   published_version_id = ?
			 WHERE id = ?`,
			a.Name, a.Description, string(a.Visibility), meta,
			nullNanos(a.StaleAt), nullNanos(a.ExpiresAt), nullNanos(a.UnpublishedAt), nullNanos(a.DeletedAt),
			toNanos(a.UpdatedAt), vid, aid)
		if err != nil {
			return err
		}
		if v.CreatedBy != "" {
			if err := markArtifactActor(ctx, tx, aid, v.CreatedBy, false, v.CreatedAt); err != nil {
				return err
			}
		}
		v.ID = vid
		return nil
	})
	if err != nil {
		return artifactErr("add version", err)
	}
	v.Number = number
	return nil
}

func (s *ArtifactStore) Update(ctx context.Context, slug, by string, fn func(a *artifact.Artifact) error) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Update")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		aid, a, err := readArtifactTx(ctx, tx, slug)
		if err != nil {
			return err
		}
		if err := fn(a); err != nil {
			return err
		}
		meta, err := encodeArtifactMetadata(a.Metadata)
		if err != nil {
			return err
		}
		ownerID, err := artifactUserID(ctx, tx, a.OwnerHandle)
		if err != nil {
			return err
		}
		var published sql.NullInt64
		if a.PublishedVersion != 0 {
			err := tx.QueryRowContext(ctx, `SELECT id FROM version WHERE artifact_id = ? AND number = ?`,
				aid, a.PublishedVersion).Scan(&published.Int64)
			if errors.Is(err, sql.ErrNoRows) {
				return artifact.ErrVersionNotFound
			}
			if err != nil {
				return err
			}
			published.Valid = true
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE artifact SET name = ?, description = ?, owner_id = ?, visibility = ?, metadata = ?,
			   published_version_id = ?, stale_at = ?, expires_at = ?, unpublished_at = ?, deleted_at = ?,
			   updated_at = ?
			 WHERE id = ?`,
			a.Name, a.Description, ownerID, string(a.Visibility), meta, published,
			nullNanos(a.StaleAt), nullNanos(a.ExpiresAt), nullNanos(a.UnpublishedAt), nullNanos(a.DeletedAt),
			toNanos(a.UpdatedAt), aid)
		if err != nil {
			return err
		}
		if by != "" {
			return markArtifactActor(ctx, tx, aid, by, false, a.UpdatedAt)
		}
		return nil
	})
	return artifactErrOrNil("update", err)
}

func (s *ArtifactStore) Involved(ctx context.Context, slug, handle string) (_ bool, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Involved")
	defer func() { end(err) }()
	var ok bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM artifact_actor aa
		   JOIN artifact a ON a.id = aa.artifact_id
		   JOIN user u ON u.id = aa.user_id
		   WHERE a.slug = ? AND u.handle = ?)`, slug, handle).Scan(&ok)
	if err != nil {
		return false, artifactErr("involved", err)
	}
	return ok, nil
}

func artifactErrOrNil(op string, err error) error {
	if err == nil {
		return nil
	}
	return artifactErr(op, err)
}

type artifactCursor struct {
	UpdatedAt int64 `json:"u"`
	ID        int64 `json:"i"`
}

func encodeArtifactCursor(c artifactCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeArtifactCursor(s string) (artifactCursor, error) {
	var c artifactCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.ID <= 0 {
		return c, artifact.Errorf(artifact.ErrInvalidArgument, "invalid cursor")
	}
	return c, nil
}

// List shows a viewer without All what it owns, created or updated, plus
// public and members artifacts that are neither unpublished nor deleted.
func (s *ArtifactStore) List(ctx context.Context, q artifact.ListQuery) (_ artifact.ListPage, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.List")
	defer func() { end(err) }()
	var (
		where = []string{"a.purged_at IS NULL"}
		args  []any
	)
	involved := func(cond string) string {
		return `EXISTS (SELECT 1 FROM artifact_actor aa JOIN user au ON au.id = aa.user_id
			WHERE aa.artifact_id = a.id AND au.handle = ?` + cond + `)`
	}
	if !q.All {
		where = append(where, `((a.visibility IN ('public', 'members') AND a.unpublished_at IS NULL AND a.deleted_at IS NULL)
			OR u.handle = ? OR `+involved("")+`)`)
		args = append(args, q.Viewer, q.Viewer)
	}
	switch q.Mine {
	case "":
	case "created":
		where = append(where, involved(" AND aa.created = 1"))
		args = append(args, q.Viewer)
	case "updated":
		where = append(where, involved(" AND aa.updated = 1"))
		args = append(args, q.Viewer)
	case "any":
		where = append(where, involved(""))
		args = append(args, q.Viewer)
	default:
		return artifact.ListPage{}, artifact.Errorf(artifact.ErrInvalidArgument, "mine must be created, updated or any")
	}
	if q.Owner != "" {
		where = append(where, "u.handle = ?")
		args = append(args, q.Owner)
	}
	keys := make([]string, 0, len(q.Metadata))
	for k := range q.Metadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := artifact.ValidateMetadataKey(k); err != nil {
			return artifact.ListPage{}, err
		}
		// Quoted so a dotted key such as kazari.spa is one member, not a
		// nested path. The key charset excludes quotes.
		where = append(where, `json_extract(a.metadata, '$."' || ? || '"') = ?`)
		args = append(args, k, q.Metadata[k])
	}
	switch q.Stale {
	case "", "include":
	case "exclude":
		where = append(where, "a.stale_at IS NULL")
	case "only":
		where = append(where, "a.stale_at IS NOT NULL")
	default:
		return artifact.ListPage{}, artifact.Errorf(artifact.ErrInvalidArgument, "stale must be include, exclude or only")
	}
	switch q.Deleted {
	case "", "exclude":
		where = append(where, "a.deleted_at IS NULL")
	case "include":
	case "only":
		where = append(where, "a.deleted_at IS NOT NULL")
	default:
		return artifact.ListPage{}, artifact.Errorf(artifact.ErrInvalidArgument, "deleted must be exclude, include or only")
	}
	if q.Cursor != "" {
		c, err := decodeArtifactCursor(q.Cursor)
		if err != nil {
			return artifact.ListPage{}, err
		}
		where = append(where, "(a.updated_at < ? OR (a.updated_at = ? AND a.id < ?))")
		args = append(args, c.UpdatedAt, c.UpdatedAt, c.ID)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = artifactListDefault
	}
	limit = min(limit, artifactListMax)
	args = append(args, limit+1)

	query := artifactSelect + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY a.updated_at DESC, a.id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return artifact.ListPage{}, artifactErr("list", err)
	}
	defer rows.Close()
	var (
		page artifact.ListPage
		ids  []int64
	)
	for rows.Next() {
		id, a, err := scanArtifact(rows)
		if err != nil {
			return artifact.ListPage{}, artifactErr("list", err)
		}
		ids = append(ids, id)
		page.Items = append(page.Items, *a)
	}
	if err := rows.Err(); err != nil {
		return artifact.ListPage{}, artifactErr("list", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeArtifactCursor(artifactCursor{UpdatedAt: toNanos(last.UpdatedAt), ID: ids[limit-1]})
	}
	return page, nil
}

const artifactResolveSelect = `SELECT a.slug, COALESCE(v.id, 0), COALESCE(v.number, 0), a.visibility, a.metadata,
	a.unpublished_at, a.deleted_at, a.purged_at`

func scanArtifactPublished(row artifactRowScanner) (*artifact.Published, error) {
	var (
		p                  artifact.Published
		vis, meta          string
		unpub, del, purged sql.NullInt64
	)
	if err := row.Scan(&p.Slug, &p.VersionID, &p.VersionNumber, &vis, &meta, &unpub, &del, &purged); err != nil {
		return nil, err
	}
	var err error
	if p.Metadata, err = decodeArtifactMetadata(meta); err != nil {
		return nil, err
	}
	p.Visibility = artifact.Visibility(vis)
	p.UnpublishedAt = ptrTime(unpub)
	p.DeletedAt = ptrTime(del)
	p.PurgedAt = ptrTime(purged)
	return &p, nil
}

func (s *ArtifactStore) ResolvePublished(ctx context.Context, slug string) (_ *artifact.Published, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ResolvePublished")
	defer func() { end(err) }()
	p, err := scanArtifactPublished(s.db.QueryRowContext(ctx, artifactResolveSelect+`
		FROM artifact a LEFT JOIN version v ON v.id = a.published_version_id
		WHERE a.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, artifactErr("resolve", err)
	}
	return p, nil
}

// ResolveVersion resolves a tombstone with VersionID 0 and PurgedAt set, so
// the caller can answer 410.
func (s *ArtifactStore) ResolveVersion(ctx context.Context, slug string, number int) (_ *artifact.Published, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ResolveVersion")
	defer func() { end(err) }()
	p, err := scanArtifactPublished(s.db.QueryRowContext(ctx, artifactResolveSelect+`
		FROM artifact a LEFT JOIN version v ON v.artifact_id = a.id AND v.number = ?
		WHERE a.slug = ?`, number, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, artifactErr("resolve version", err)
	}
	if p.VersionID == 0 && p.PurgedAt == nil {
		return nil, artifact.ErrVersionNotFound
	}
	return p, nil
}

func (s *ArtifactStore) Files(ctx context.Context, versionID int64) (_ map[string]artifact.File, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Files")
	defer func() { end(err) }()
	files, err := s.files(ctx, versionID)
	if err != nil {
		return nil, artifactErr("files", err)
	}
	m := make(map[string]artifact.File, len(files))
	for _, f := range files {
		m[f.Path] = f
	}
	return m, nil
}

func (s *ArtifactStore) Usage(ctx context.Context, ownerHandle string) (_ int64, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Usage")
	defer func() { end(err) }()
	var n int64
	err = s.db.QueryRowContext(ctx, artifactUsage, ownerHandle).Scan(&n)
	if err != nil {
		return 0, artifactErr("usage", err)
	}
	return n, nil
}

func (s *ArtifactStore) listWhere(ctx context.Context, op, cond, order string, arg any) ([]artifact.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, artifactSelect+" WHERE "+cond+" ORDER BY "+order+", a.id", arg)
	if err != nil {
		return nil, artifactErr(op, err)
	}
	defer rows.Close()
	var out []artifact.Artifact
	for rows.Next() {
		_, a, err := scanArtifact(rows)
		if err != nil {
			return nil, artifactErr(op, err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, artifactErr(op, err)
	}
	return out, nil
}

func (s *ArtifactStore) ListExpired(ctx context.Context, now time.Time) (_ []artifact.Artifact, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ListExpired")
	defer func() { end(err) }()
	return s.listWhere(ctx, "list expired",
		"a.expires_at <= ? AND a.deleted_at IS NULL AND a.purged_at IS NULL", "a.expires_at", toNanos(now))
}

func (s *ArtifactStore) ListRetentionDue(ctx context.Context, before time.Time) (_ []artifact.Artifact, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ListRetentionDue")
	defer func() { end(err) }()
	return s.listWhere(ctx, "list retention due",
		"a.deleted_at <= ? AND a.purged_at IS NULL", "a.deleted_at", toNanos(before))
}

func (s *ArtifactStore) ListTombstonesDue(ctx context.Context, before time.Time) (_ []artifact.Artifact, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.ListTombstonesDue")
	defer func() { end(err) }()
	return s.listWhere(ctx, "list tombstones due", "a.purged_at <= ?", "a.purged_at", toNanos(before))
}

// Purge also sets deleted_at if unset, so a hard delete leaves the same
// tombstone as the sweep. Purging again keeps the original purged_at.
func (s *ArtifactStore) Purge(ctx context.Context, slug string, at time.Time, fn func(a *artifact.Artifact) error) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.Purge")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		aid, a, err := readArtifactTx(ctx, tx, slug)
		if err != nil {
			return err
		}
		if fn != nil {
			if err := fn(a); err != nil {
				return err
			}
		}
		ts := toNanos(at)
		if _, err := tx.ExecContext(ctx,
			`UPDATE artifact SET published_version_id = NULL,
			   purged_at = COALESCE(purged_at, ?), deleted_at = COALESCE(deleted_at, ?)
			 WHERE id = ?`, ts, ts, aid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM version_file WHERE version_id IN (SELECT id FROM version WHERE artifact_id = ?)`, aid); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM version WHERE artifact_id = ?`, aid)
		return err
	})
	return artifactErrOrNil("purge", err)
}

// DeleteTombstone returns ErrInvalidArgument unless the artifact is purged.
func (s *ArtifactStore) DeleteTombstone(ctx context.Context, slug string) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.DeleteTombstone")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var purged sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT purged_at FROM artifact WHERE slug = ?`, slug).Scan(&purged)
		if errors.Is(err, sql.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !purged.Valid {
			return artifact.Errorf(artifact.ErrInvalidArgument, "artifact %q is not a tombstone", slug)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM artifact WHERE slug = ?`, slug)
		return err
	})
	return artifactErrOrNil("delete tombstone", err)
}

// RecordBlob only moves created_at forward.
func (s *ArtifactStore) RecordBlob(ctx context.Context, hash string, size int64, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.RecordBlob")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, artifactUpsertBlob, hash, size, toNanos(at))
	return artifactErrOrNil("record blob", err)
}

func (s *ArtifactStore) UnreferencedBlobs(ctx context.Context, before time.Time, limit int) (_ []string, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.UnreferencedBlobs")
	defer func() { end(err) }()
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.hash FROM blob b
		 WHERE b.created_at < ?
		   AND NOT EXISTS (SELECT 1 FROM version_file f WHERE f.blob_hash = b.hash)
		 ORDER BY b.created_at, b.hash LIMIT ?`, toNanos(before), limit)
	if err != nil {
		return nil, artifactErr("unreferenced blobs", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, artifactErr("unreferenced blobs", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, artifactErr("unreferenced blobs", err)
	}
	return out, nil
}

func (s *ArtifactStore) DeleteBlob(ctx context.Context, hash string) (err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.DeleteBlob")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `DELETE FROM blob WHERE hash = ?`, hash)
	return artifactErrOrNil("delete blob", err)
}

// artifactStatus applies the lifecycle's precedence, so each artifact has
// exactly one status.
const artifactStatus = `CASE
	WHEN purged_at IS NOT NULL THEN 'purged'
	WHEN deleted_at IS NOT NULL THEN 'deleted'
	WHEN unpublished_at IS NOT NULL THEN 'unpublished'
	WHEN stale_at IS NOT NULL THEN 'stale'
	ELSE 'published'
END`

func (s *ArtifactStore) CountArtifacts(ctx context.Context) (_ []artifact.StatusCount, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.CountArtifacts")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx, `SELECT `+artifactStatus+`, visibility, COUNT(*) FROM artifact GROUP BY 1, 2`)
	if err != nil {
		return nil, artifactErr("count", err)
	}
	defer rows.Close()
	var out []artifact.StatusCount
	for rows.Next() {
		var c artifact.StatusCount
		if err := rows.Scan(&c.Status, &c.Visibility, &c.Count); err != nil {
			return nil, artifactErr("count", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, artifactErr("count", err)
	}
	return out, nil
}

func (s *ArtifactStore) VersionTotals(ctx context.Context) (_ artifact.Totals, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.VersionTotals")
	defer func() { end(err) }()
	return s.totals(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM version`)
}

func (s *ArtifactStore) BlobTotals(ctx context.Context) (_ artifact.Totals, err error) {
	ctx, end := s.db.op(ctx, "ArtifactStore.BlobTotals")
	defer func() { end(err) }()
	return s.totals(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blob`)
}

func (s *ArtifactStore) totals(ctx context.Context, query string) (artifact.Totals, error) {
	var t artifact.Totals
	if err := s.db.QueryRowContext(ctx, query).Scan(&t.Count, &t.Bytes); err != nil {
		return artifact.Totals{}, artifactErr("totals", err)
	}
	return t, nil
}
