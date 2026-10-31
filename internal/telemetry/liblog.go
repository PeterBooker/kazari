package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"
)

// CaptureLibraryLogs sends what the OpenTelemetry SDK and gRPC log by
// themselves to l; left alone, both write plain text to stderr whatever
// log.format says. Their errors are telemetry problems, logged at warn like
// Kazari's own, and the rest is for debugging. Both loggers are process-wide
// and gRPC's must be set before its first use, so serve calls this once,
// before New.
func CaptureLibraryLogs(l *slog.Logger) {
	otel.SetLogger(otelLogger(l))
	grpclog.SetLoggerV2(grpcLogger{l.With("logger", "grpc")})
}

func otelLogger(l *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(sdkLevels{l.Handler()}).WithName("otel")
}

// sdkLevels maps the SDK's logr verbosity, which FromSlogHandler passes as
// negative slog levels: its warnings (V 1) are debug, and its info (V 4), a
// line for every meter and tracer created, and debug (V 8), which dumps
// whole metric batches, are dropped as its default logger drops them.
type sdkLevels struct{ slog.Handler }

func sdkLevel(l slog.Level) (slog.Level, bool) {
	switch {
	case l >= slog.LevelError:
		return slog.LevelWarn, true
	case l > slog.LevelDebug:
		return slog.LevelDebug, true
	}
	return 0, false
}

func (h sdkLevels) Enabled(ctx context.Context, l slog.Level) bool {
	l, ok := sdkLevel(l)
	return ok && h.Handler.Enabled(ctx, l)
}

// Handle checks the level itself: logr writes errors without asking Enabled.
func (h sdkLevels) Handle(ctx context.Context, r slog.Record) error {
	l, ok := sdkLevel(r.Level)
	if !ok || !h.Handler.Enabled(ctx, l) {
		return nil
	}
	r.Level = l
	return h.Handler.Handle(ctx, r)
}

func (h sdkLevels) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sdkLevels{h.Handler.WithAttrs(attrs)}
}

func (h sdkLevels) WithGroup(name string) slog.Handler { return sdkLevels{h.Handler.WithGroup(name)} }

// grpcLogger logs gRPC's warnings, a line for every reconnection attempt, at
// debug: the export wrappers already report a collector that cannot be
// reached. Its info, every change of connection state, is dropped as its
// default logger drops it.
type grpcLogger struct{ log *slog.Logger }

func (g grpcLogger) print(level slog.Level, format func(...any) string, args []any) {
	if g.log.Enabled(context.Background(), level) {
		g.log.Log(context.Background(), level, strings.TrimSuffix(format(args...), "\n"))
	}
}

func sprintf(format string) func(...any) string {
	return func(args ...any) string { return fmt.Sprintf(format, args...) }
}

func (grpcLogger) Info(...any)             {}
func (grpcLogger) Infoln(...any)           {}
func (grpcLogger) Infof(string, ...any)    {}
func (g grpcLogger) Warning(args ...any)   { g.print(slog.LevelDebug, fmt.Sprint, args) }
func (g grpcLogger) Warningln(args ...any) { g.print(slog.LevelDebug, fmt.Sprintln, args) }
func (g grpcLogger) Warningf(format string, args ...any) {
	g.print(slog.LevelDebug, sprintf(format), args)
}
func (g grpcLogger) Error(args ...any)   { g.print(slog.LevelWarn, fmt.Sprint, args) }
func (g grpcLogger) Errorln(args ...any) { g.print(slog.LevelWarn, fmt.Sprintln, args) }
func (g grpcLogger) Errorf(format string, args ...any) {
	g.print(slog.LevelWarn, sprintf(format), args)
}

// The Fatal methods exit, as grpclog requires of a logger.
func (g grpcLogger) Fatal(args ...any)                 { g.fatal(fmt.Sprint, args) }
func (g grpcLogger) Fatalln(args ...any)               { g.fatal(fmt.Sprintln, args) }
func (g grpcLogger) Fatalf(format string, args ...any) { g.fatal(sprintf(format), args) }

func (g grpcLogger) fatal(format func(...any) string, args []any) {
	g.print(slog.LevelError, format, args)
	os.Exit(1)
}

// V keeps gRPC's default verbosity, 0, as GRPC_GO_LOG_VERBOSITY_LEVEL no
// longer applies.
func (g grpcLogger) V(l int) bool { return l <= 0 }
