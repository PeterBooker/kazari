package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/views"
)

// ViewStore implements views.Store.
type ViewStore struct {
	db *DB
}

var _ views.Store = (*ViewStore)(nil)

// NewViewStore returns a view store backed by db.
func NewViewStore(db *DB) *ViewStore {
	return &ViewStore{db: db}
}

func viewErr(op string, err error) error {
	if err == nil || artifact.CodeOf(err) != "" {
		return err
	}
	return fmt.Errorf("sqlite: %s: %w", op, err)
}

// AddViews skips counts for an artifact that is gone, and readers whose
// handle no longer resolves.
func (s *ViewStore) AddViews(ctx context.Context, b views.Batch) (err error) {
	ctx, end := s.db.op(ctx, "ViewStore.AddViews")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		days, err := tx.PrepareContext(ctx,
			`INSERT INTO view_day (artifact_id, day, views, visitors, member_views, link_views)
			 SELECT id, ?, ?, ?, ?, ? FROM artifact WHERE id = ? AND purged_at IS NULL
			 ON CONFLICT (artifact_id, day) DO UPDATE SET
			   views = views + excluded.views, visitors = visitors + excluded.visitors,
			   member_views = member_views + excluded.member_views, link_views = link_views + excluded.link_views`)
		if err != nil {
			return err
		}
		defer days.Close()
		for _, d := range b.Days {
			if _, err := days.ExecContext(ctx, toNanos(d.Day), d.Views, d.Visitors, d.MemberViews, d.LinkViews, d.ArtifactID); err != nil {
				return err
			}
		}
		paths, err := tx.PrepareContext(ctx,
			`INSERT INTO view_path_day (artifact_id, day, path, views)
			 SELECT id, ?, ?, ? FROM artifact WHERE id = ? AND purged_at IS NULL
			 ON CONFLICT (artifact_id, day, path) DO UPDATE SET views = views + excluded.views`)
		if err != nil {
			return err
		}
		defer paths.Close()
		for _, p := range b.Paths {
			if _, err := paths.ExecContext(ctx, toNanos(p.Day), p.Path, p.Views, p.ArtifactID); err != nil {
				return err
			}
		}
		readers, err := tx.PrepareContext(ctx,
			`INSERT INTO view_reader (artifact_id, user_id, first_at, last_at, views)
			 SELECT a.id, u.id, ?, ?, ? FROM artifact a, user u
			 WHERE a.id = ? AND a.purged_at IS NULL AND u.handle = ?
			 ON CONFLICT (artifact_id, user_id) DO UPDATE SET
			   first_at = min(first_at, excluded.first_at), last_at = max(last_at, excluded.last_at),
			   views = views + excluded.views`)
		if err != nil {
			return err
		}
		defer readers.Close()
		for _, r := range b.Readers {
			if _, err := readers.ExecContext(ctx, toNanos(r.FirstAt), toNanos(r.LastAt), r.Views, r.ArtifactID, r.Handle); err != nil {
				return err
			}
		}
		return nil
	})
	return viewErr("add views", err)
}

// PruneViews implements views.Store.
func (s *ViewStore) PruneViews(ctx context.Context, before time.Time) (n int64, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.PruneViews")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, table := range []string{"view_day", "view_path_day"} {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day < ?`, toNanos(before))
			if err != nil {
				return err
			}
			rows, err := res.RowsAffected()
			if err != nil {
				return err
			}
			n += rows
		}
		return nil
	})
	return n, viewErr("prune views", err)
}

// InstanceDays implements views.Store.
func (s *ViewStore) InstanceDays(ctx context.Context, from, to time.Time) (_ []views.DayTotals, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.InstanceDays")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, sum(views), sum(visitors), sum(member_views), sum(link_views)
		 FROM view_day WHERE day >= ? AND day < ? GROUP BY day ORDER BY day`, toNanos(from), toNanos(to))
	if err != nil {
		return nil, viewErr("instance views", err)
	}
	out, err := scanDays(rows)
	return out, viewErr("instance views", err)
}

// TopArtifacts implements views.Store; ties go to the slug first in order.
func (s *ViewStore) TopArtifacts(ctx context.Context, from, to time.Time, limit int) (_ []views.ArtifactTotals, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.TopArtifacts")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.slug, sum(d.views), sum(d.visitors), sum(d.member_views), sum(d.link_views)
		 FROM view_day d JOIN artifact a ON a.id = d.artifact_id
		 WHERE d.day >= ? AND d.day < ? AND a.purged_at IS NULL
		 GROUP BY a.id ORDER BY sum(d.views) DESC, a.slug LIMIT ?`, toNanos(from), toNanos(to), limit)
	if err != nil {
		return nil, viewErr("top artifacts", err)
	}
	defer rows.Close()
	var out []views.ArtifactTotals
	for rows.Next() {
		var a views.ArtifactTotals
		if err := rows.Scan(&a.Slug, &a.Views, &a.Visitors, &a.MemberViews, &a.LinkViews); err != nil {
			return nil, viewErr("top artifacts", err)
		}
		out = append(out, a)
	}
	return out, viewErr("top artifacts", rows.Err())
}

// ArtifactSums implements views.Store.
func (s *ViewStore) ArtifactSums(ctx context.Context, slugs []string, from, to time.Time) (_ map[string]views.Totals, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.ArtifactSums")
	defer func() { end(err) }()
	out := make(map[string]views.Totals, len(slugs))
	if len(slugs) == 0 {
		return out, nil
	}
	args := []any{toNanos(from), toNanos(to)}
	for _, slug := range slugs {
		args = append(args, slug)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.slug, sum(d.views), sum(d.visitors), sum(d.member_views), sum(d.link_views)
		 FROM view_day d JOIN artifact a ON a.id = d.artifact_id
		 WHERE d.day >= ? AND d.day < ? AND a.slug IN (?`+strings.Repeat(", ?", len(slugs)-1)+`)
		 GROUP BY a.id`, args...)
	if err != nil {
		return nil, viewErr("artifact views", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			slug string
			t    views.Totals
		)
		if err := rows.Scan(&slug, &t.Views, &t.Visitors, &t.MemberViews, &t.LinkViews); err != nil {
			return nil, viewErr("artifact views", err)
		}
		out[slug] = t
	}
	return out, viewErr("artifact views", rows.Err())
}

// artifactID returns artifact.ErrNotFound for a slug that names no artifact.
func (s *ViewStore) artifactID(ctx context.Context, slug string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM artifact WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, artifact.ErrNotFound
	}
	return id, err
}

// ArtifactDays implements views.Store.
func (s *ViewStore) ArtifactDays(ctx context.Context, slug string, from, to time.Time) (_ []views.DayTotals, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.ArtifactDays")
	defer func() { end(err) }()
	id, err := s.artifactID(ctx, slug)
	if err != nil {
		return nil, viewErr("artifact days", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, views, visitors, member_views, link_views
		 FROM view_day WHERE artifact_id = ? AND day >= ? AND day < ? ORDER BY day`, id, toNanos(from), toNanos(to))
	if err != nil {
		return nil, viewErr("artifact days", err)
	}
	out, err := scanDays(rows)
	return out, viewErr("artifact days", err)
}

// TopPaths implements views.Store; ties go to the path first in order.
func (s *ViewStore) TopPaths(ctx context.Context, slug string, from, to time.Time, limit int) (_ []views.FileViews, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.TopPaths")
	defer func() { end(err) }()
	id, err := s.artifactID(ctx, slug)
	if err != nil {
		return nil, viewErr("top paths", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, sum(views) FROM view_path_day
		 WHERE artifact_id = ? AND day >= ? AND day < ?
		 GROUP BY path ORDER BY sum(views) DESC, path LIMIT ?`, id, toNanos(from), toNanos(to), limit)
	if err != nil {
		return nil, viewErr("top paths", err)
	}
	defer rows.Close()
	var out []views.FileViews
	for rows.Next() {
		var f views.FileViews
		if err := rows.Scan(&f.Path, &f.Views); err != nil {
			return nil, viewErr("top paths", err)
		}
		out = append(out, f)
	}
	return out, viewErr("top paths", rows.Err())
}

// Readers implements views.Store, the latest reader first.
func (s *ViewStore) Readers(ctx context.Context, slug string, limit int) (_ []views.Reader, err error) {
	ctx, end := s.db.op(ctx, "ViewStore.Readers")
	defer func() { end(err) }()
	id, err := s.artifactID(ctx, slug)
	if err != nil {
		return nil, viewErr("readers", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.handle, u.display_name, r.first_at, r.last_at, r.views
		 FROM view_reader r JOIN user u ON u.id = r.user_id
		 WHERE r.artifact_id = ? ORDER BY r.last_at DESC, u.handle LIMIT ?`, id, limit)
	if err != nil {
		return nil, viewErr("readers", err)
	}
	defer rows.Close()
	var out []views.Reader
	for rows.Next() {
		var (
			r           views.Reader
			first, last int64
		)
		if err := rows.Scan(&r.Handle, &r.Name, &first, &last, &r.Views); err != nil {
			return nil, viewErr("readers", err)
		}
		r.FirstAt, r.LastAt = fromNanos(first), fromNanos(last)
		out = append(out, r)
	}
	return out, viewErr("readers", rows.Err())
}

func scanDays(rows *sql.Rows) ([]views.DayTotals, error) {
	defer rows.Close()
	var out []views.DayTotals
	for rows.Next() {
		var (
			d   views.DayTotals
			day int64
		)
		if err := rows.Scan(&day, &d.Views, &d.Visitors, &d.MemberViews, &d.LinkViews); err != nil {
			return nil, err
		}
		d.Day = fromNanos(day)
		out = append(out, d)
	}
	return out, rows.Err()
}
