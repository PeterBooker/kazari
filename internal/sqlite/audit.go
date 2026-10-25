package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kazari/internal/audit"
)

// AuditStore implements audit.Recorder and audit.Reader.
type AuditStore struct {
	db *sql.DB
}

var (
	_ audit.Recorder = (*AuditStore)(nil)
	_ audit.Reader   = (*AuditStore)(nil)
)

// NewAuditStore returns an audit store backed by db.
func NewAuditStore(db *DB) *AuditStore {
	return &AuditStore{db: db.DB}
}

// Record stamps a zero e.At with the current time and ignores e.ID.
func (s *AuditStore) Record(ctx context.Context, e audit.Event) error {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_event (at, actor, action, slug, details)
		VALUES (?, ?, ?, ?, ?)`, toNanos(e.At), e.Actor, e.Action, e.Slug, string(details))
	if err != nil {
		return fmt.Errorf("sqlite: record audit %s: %w", e.Action, err)
	}
	return nil
}

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

// List treats Since as inclusive; Slug and Actor match exactly.
func (s *AuditStore) List(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	var (
		where []string
		args  []any
	)
	if !q.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, toNanos(q.Since))
	}
	if q.Slug != "" {
		where = append(where, "slug = ?")
		args = append(args, q.Slug)
	}
	if q.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, q.Actor)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditLimit
	case limit > maxAuditLimit:
		limit = maxAuditLimit
	}
	query := `SELECT id, at, actor, action, slug, details FROM audit_event`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list audit: %w", err)
	}
	defer rows.Close()
	var events []audit.Event
	for rows.Next() {
		var (
			e       audit.Event
			at      int64
			details string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Slug, &details); err != nil {
			return nil, fmt.Errorf("sqlite: list audit: %w", err)
		}
		e.At = fromNanos(at)
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			return nil, fmt.Errorf("sqlite: list audit: event %d details: %w", e.ID, err)
		}
		if len(e.Details) == 0 {
			e.Details = nil
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list audit: %w", err)
	}
	return events, nil
}
