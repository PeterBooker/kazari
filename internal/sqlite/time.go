package sqlite

import (
	"database/sql"
	"time"
)

// Timestamps are stored as INTEGER unix nanoseconds. Integers compare
// correctly in SQL, round-trip exactly, and need no format parsing.

func nowNanos() int64 { return time.Now().UnixNano() }

func toNanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

// nullNanos converts an optional time for binding.
func nullNanos(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

// ptrTime converts a scanned nullable column back to *time.Time.
func ptrTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}
