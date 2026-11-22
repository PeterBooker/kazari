package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
)

// AuditStore implements audit.Recorder and audit.Reader.
type AuditStore struct {
	db *DB
}

var (
	_ audit.Recorder = (*AuditStore)(nil)
	_ audit.Reader   = (*AuditStore)(nil)
)

// NewAuditStore returns an audit store backed by db.
func NewAuditStore(db *DB) *AuditStore {
	return &AuditStore{db: db}
}

// Record stamps a zero e.At with the current time and ignores e.ID.
func (s *AuditStore) Record(ctx context.Context, e audit.Event) (err error) {
	ctx, end := s.db.op(ctx, "AuditStore.Record")
	defer func() { end(err) }()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	details := []byte("{}")
	if len(e.Details) > 0 {
		var err error
		if details, err = json.Marshal(e.Details); err != nil {
			return fmt.Errorf("sqlite: record audit %s: %w", e.Action, err)
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_event (at, actor, action, slug, subject, details)
		VALUES (?, ?, ?, ?, ?, ?)`, toNanos(e.At), e.Actor, e.Action, e.Slug, e.Subject, string(details))
	if err != nil {
		return fmt.Errorf("sqlite: record audit %s: %w", e.Action, err)
	}
	return nil
}

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

type auditCursor struct {
	At int64 `json:"a"`
	ID int64 `json:"i"`
}

func encodeAuditCursor(c auditCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAuditCursor(s string) (auditCursor, error) {
	var c auditCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.ID <= 0 {
		return c, fmt.Errorf("%w: invalid cursor", audit.ErrInvalidQuery)
	}
	return c, nil
}

// List matches Slug, Actor and Subject exactly and pages by (at, id), so events
// recorded in the same nanosecond are neither repeated nor skipped.
func (s *AuditStore) List(ctx context.Context, q audit.Query) (_ audit.Page, err error) {
	ctx, end := s.db.op(ctx, "AuditStore.List")
	defer func() { end(err) }()
	if err := q.Validate(); err != nil {
		return audit.Page{}, err
	}
	var (
		where []string
		args  []any
	)
	if !q.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, toNanos(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "at < ?")
		args = append(args, toNanos(q.Until))
	}
	if q.Slug != "" {
		where = append(where, "slug = ?")
		args = append(args, q.Slug)
	}
	if q.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, q.Actor)
	}
	if q.Subject != "" {
		where = append(where, "subject = ?")
		args = append(args, q.Subject)
	}
	if len(q.Actions) > 0 {
		conds := make([]string, 0, len(q.Actions))
		for _, a := range q.Actions {
			if strings.HasSuffix(a, ".") {
				// substr rather than LIKE: no character of the prefix is
				// ever read as a wildcard.
				conds = append(conds, "substr(action, 1, ?) = ?")
				args = append(args, len(a), a)
				continue
			}
			conds = append(conds, "action = ?")
			args = append(args, a)
		}
		where = append(where, "("+strings.Join(conds, " OR ")+")")
	}
	if q.Cursor != "" {
		c, err := decodeAuditCursor(q.Cursor)
		if err != nil {
			return audit.Page{}, err
		}
		where = append(where, "(at < ? OR (at = ? AND id < ?))")
		args = append(args, c.At, c.At, c.ID)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditLimit
	case limit > maxAuditLimit:
		limit = maxAuditLimit
	}
	query := `SELECT id, at, actor, action, slug, subject, details FROM audit_event`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return audit.Page{}, fmt.Errorf("sqlite: list audit: %w", err)
	}
	defer rows.Close()
	var page audit.Page
	for rows.Next() {
		var (
			e       audit.Event
			at      int64
			details string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Slug, &e.Subject, &details); err != nil {
			return audit.Page{}, fmt.Errorf("sqlite: list audit: %w", err)
		}
		e.At = fromNanos(at)
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			return audit.Page{}, fmt.Errorf("sqlite: list audit: event %d details: %w", e.ID, err)
		}
		if len(e.Details) == 0 {
			e.Details = nil
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return audit.Page{}, fmt.Errorf("sqlite: list audit: %w", err)
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		last := page.Events[limit-1]
		page.NextCursor = encodeAuditCursor(auditCursor{At: toNanos(last.At), ID: last.ID})
	}
	return page, nil
}
