package certs

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newZapLogger adapts l for CertMagic, which logs through zap.
func newZapLogger(l *slog.Logger) *zap.Logger {
	return zap.New(slogCore{h: l.Handler()})
}

// slogCore is a zapcore.Core that forwards entries to a slog.Handler.
type slogCore struct {
	h slog.Handler
}

func (c slogCore) Enabled(lvl zapcore.Level) bool {
	return c.h.Enabled(context.Background(), slogLevel(lvl))
}

func (c slogCore) With(fields []zapcore.Field) zapcore.Core {
	return slogCore{h: c.h.WithAttrs(slogAttrs(fields))}
}

func (c slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c slogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	r := slog.NewRecord(e.Time, slogLevel(e.Level), e.Message, 0)
	if e.LoggerName != "" {
		r.AddAttrs(slog.String("logger", e.LoggerName))
	}
	r.AddAttrs(slogAttrs(fields)...)
	return c.h.Handle(context.Background(), r)
}

func (c slogCore) Sync() error { return nil }

func slogLevel(l zapcore.Level) slog.Level {
	switch {
	case l <= zapcore.DebugLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// slogAttrs encodes zap fields to attrs, sorted by key for stable output.
func slogAttrs(fields []zapcore.Field) []slog.Attr {
	if len(fields) == 0 {
		return nil
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(enc.Fields))
	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	slices.SortFunc(attrs, func(a, b slog.Attr) int { return strings.Compare(a.Key, b.Key) })
	return attrs
}
