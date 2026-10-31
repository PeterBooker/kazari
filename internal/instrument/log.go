package instrument

import (
	"context"
	"log/slog"
)

// MinLevel returns h with every record below min dropped, for a sink that
// wants less than the logger it shares a handler with.
func MinLevel(h slog.Handler, min slog.Leveler) slog.Handler {
	return minLevel{min: min, Handler: h}
}

type minLevel struct {
	min slog.Leveler
	slog.Handler
}

func (h minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min.Level() && h.Handler.Enabled(ctx, l)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{min: h.min, Handler: h.Handler.WithAttrs(attrs)}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{min: h.min, Handler: h.Handler.WithGroup(name)}
}
