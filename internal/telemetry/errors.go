package telemetry

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
)

// logEvery spaces repeated warnings: an unreachable collector fails every
// export, and one line a minute says so as clearly as thousands.
const logEvery = time.Minute

// limitedLog writes at most one warning per key every logEvery, counting the
// ones it held back.
type limitedLog struct {
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int
}

func newLimitedLog(log *slog.Logger, now func() time.Time) *limitedLog {
	return &limitedLog{log: log, now: now, last: map[string]time.Time{}, held: map[string]int{}}
}

func (l *limitedLog) warn(key, msg string, args ...any) {
	now := l.now()
	l.mu.Lock()
	if last, ok := l.last[key]; ok && now.Sub(last) < logEvery {
		l.held[key]++
		l.mu.Unlock()
		return
	}
	held := l.held[key]
	l.last[key] = now
	l.held[key] = 0
	l.mu.Unlock()
	if held > 0 {
		args = append(args, "suppressed", held)
	}
	l.log.Warn(msg, args...)
}

// exportError marks an error the export wrappers already logged, so the
// SDK's error handler, which the readers and processors pass it to, does
// not log it again.
type exportError struct{ err error }

func (e *exportError) Error() string { return e.err.Error() }
func (e *exportError) Unwrap() error { return e.err }

var (
	handlerOnce sync.Once
	// handlerLog is the latest New's log: tests build many in parallel,
	// while otel.SetErrorHandler is process-wide.
	handlerLog atomic.Pointer[limitedLog]
)

func installErrorHandler(l *limitedLog) {
	handlerLog.Store(l)
	handlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			var logged *exportError
			if errors.As(err, &logged) {
				return
			}
			if l := handlerLog.Load(); l != nil {
				l.warn("sdk", "telemetry: OpenTelemetry error", "err", err)
			}
		}))
	})
}
