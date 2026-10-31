package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func leveledLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
}

// lines returns each record's level, message and the attributes named.
func lines(t *testing.T, buf *bytes.Buffer, keys ...string) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(buf.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		s := rec["level"].(string) + " " + rec["msg"].(string)
		for _, k := range keys {
			if v, ok := rec[k]; ok {
				s += " " + k + "=" + v.(string)
			}
		}
		out = append(out, s)
	}
	return out
}

func TestOTelLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := otelLogger(leveledLogger(&buf, slog.LevelDebug))
	l.Error(errors.New("bad duration"), "parse duration", "input", "soon")
	l.V(1).Info("limit reached: dropping trace Span attributes")
	l.V(4).Info("TracerProvider created")
	l.V(8).Info("PeriodicReader collection")

	want := []string{
		"WARN parse duration logger=otel err=bad duration input=soon",
		"DEBUG limit reached: dropping trace Span attributes logger=otel",
	}
	if diff := cmp.Diff(want, lines(t, &buf, "logger", "err", "input")); diff != "" {
		t.Errorf("records (-want +got):\n%s", diff)
	}
}

func TestOTelLoggerRespectsLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := otelLogger(leveledLogger(&buf, slog.LevelError))
	l.Error(errors.New("bad duration"), "parse duration")
	l.V(1).Info("limit reached: dropping trace Span attributes")
	if buf.Len() != 0 {
		t.Errorf("logged below log.level: %s", buf.String())
	}
}

func TestGRPCLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	g := grpcLogger{leveledLogger(&buf, slog.LevelDebug).With("logger", "grpc")}
	g.Errorf("[transport] GoAway with %s", "ENHANCE_YOUR_CALM")
	g.Warningln("[core] failed to connect:", "connection refused")
	g.Infof("[core] state changed to %s", "READY")

	want := []string{
		"WARN [transport] GoAway with ENHANCE_YOUR_CALM logger=grpc",
		"DEBUG [core] failed to connect: connection refused logger=grpc",
	}
	if diff := cmp.Diff(want, lines(t, &buf, "logger")); diff != "" {
		t.Errorf("records (-want +got):\n%s", diff)
	}
	if !g.V(0) || g.V(2) {
		t.Errorf("V(0), V(2) = %v, %v; want gRPC's default verbosity 0", g.V(0), g.V(2))
	}
}

func TestGRPCLoggerRespectsLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	g := grpcLogger{leveledLogger(&buf, slog.LevelInfo)}
	g.Warning("[core] connection refused")
	if buf.Len() != 0 {
		t.Errorf("logged a gRPC warning at log.level info: %s", buf.String())
	}
}

func TestScrapeLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := scrapeLog{newLimitedLog(leveledLogger(&buf, slog.LevelInfo), time.Now)}
	l.Println("error gathering metrics:", errors.New("collected metric was collected before"))
	l.Println("error gathering metrics:", errors.New("again"))

	want := []string{"WARN telemetry: metrics scrape failed err=error gathering metrics: collected metric was collected before"}
	if diff := cmp.Diff(want, lines(t, &buf, "err")); diff != "" {
		t.Errorf("records (-want +got):\n%s", diff)
	}
}
