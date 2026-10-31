package config

import (
	"context"
	"io"
	"log/slog"
)

// priorityHandler starts each line with its syslog priority, <3> to <7>,
// which journald strips and records, so journalctl -p filters Kazari's log
// by level instead of filing every line as info. It holds one handler per
// priority: error, warning, info and debug.
type priorityHandler [4]slog.Handler

var priorities = [4]string{"<3>", "<4>", "<6>", "<7>"}

func newPriorityHandler(w io.Writer, handler func(io.Writer) slog.Handler) priorityHandler {
	var h priorityHandler
	for i, p := range priorities {
		h[i] = handler(prefixWriter{w, p})
	}
	return h
}

func (h priorityHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h[0].Enabled(ctx, l)
}

func (h priorityHandler) Handle(ctx context.Context, r slog.Record) error {
	i := 3
	switch {
	case r.Level >= slog.LevelError:
		i = 0
	case r.Level >= slog.LevelWarn:
		i = 1
	case r.Level >= slog.LevelInfo:
		i = 2
	}
	return h[i].Handle(ctx, r)
}

func (h priorityHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for i := range h {
		h[i] = h[i].WithAttrs(attrs)
	}
	return h
}

func (h priorityHandler) WithGroup(name string) slog.Handler {
	for i := range h {
		h[i] = h[i].WithGroup(name)
	}
	return h
}

// prefixWriter writes the prefix and the line in one Write: the four
// handlers lock separately, and two writes could let another line in
// between.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if _, err := p.w.Write(append([]byte(p.prefix), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}
