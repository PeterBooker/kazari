package certs

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// recordHandler is a slog.Handler that keeps every record it handles.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   []slog.Attr
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(h.attrs...)
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}

func (h *recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Shares the record list with the parent so tests can read one place.
	return &withAttrs{parent: h, attrs: attrs}
}

func (h *recordHandler) WithGroup(string) slog.Handler { return h }

// has reports whether a record with the given message has been handled.
func (h *recordHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return true
		}
	}
	return false
}

func (h *recordHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

type withAttrs struct {
	parent *recordHandler
	attrs  []slog.Attr
}

func (w *withAttrs) Enabled(ctx context.Context, l slog.Level) bool { return true }

func (w *withAttrs) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(w.attrs...)
	return w.parent.Handle(ctx, r)
}

func (w *withAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &withAttrs{parent: w.parent, attrs: append(append([]slog.Attr(nil), w.attrs...), attrs...)}
}

func (w *withAttrs) WithGroup(string) slog.Handler { return w }

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
