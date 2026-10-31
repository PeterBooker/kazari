package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/instrument"
)

var fixed = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	return Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixed },
	}
}

func counter(n *atomic.Int64) func(context.Context) error {
	return func(context.Context) error {
		n.Add(1)
		return nil
	}
}

func TestTriggerAndStatus(t *testing.T) {
	var a, b atomic.Int64
	s := New(testOptions(),
		Job{Name: "b", Every: time.Hour, Run: counter(&b)},
		Job{Name: "a", Every: time.Minute, Run: counter(&a)},
	)
	for range 2 {
		if err := s.Trigger(t.Context(), "a"); err != nil {
			t.Fatalf("Trigger(a): %v", err)
		}
	}
	if got := a.Load(); got != 2 {
		t.Errorf("a ran %d times, want 2", got)
	}
	if got := b.Load(); got != 0 {
		t.Errorf("b ran %d times, want 0", got)
	}

	st := s.Status()
	if len(st) != 2 || st[0].Name != "a" || st[1].Name != "b" {
		t.Fatalf("Status names = %v, want [a b]", st)
	}
	if st[0].Every != time.Minute || st[1].Every != time.Hour {
		t.Errorf("Every = %v, %v", st[0].Every, st[1].Every)
	}
	if st[0].LastRun == nil || !st[0].LastRun.Equal(fixed) {
		t.Errorf("a LastRun = %v, want %v", st[0].LastRun, fixed)
	}
	if st[0].LastError != "" || st[0].Running || st[0].NextRun != nil {
		t.Errorf("a status = %+v", st[0])
	}
	if st[1].LastRun != nil {
		t.Errorf("b LastRun = %v, want nil", st[1].LastRun)
	}
}

func TestTriggerUnknown(t *testing.T) {
	s := New(testOptions())
	if err := s.Trigger(t.Context(), "nope"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Trigger = %v, want ErrUnknownJob", err)
	}
}

func TestTriggerError(t *testing.T) {
	errBoom := errors.New("boom")
	var fail atomic.Bool
	fail.Store(true)
	s := New(testOptions(), Job{Name: "j", Every: time.Hour, Run: func(context.Context) error {
		if fail.Load() {
			return errBoom
		}
		return nil
	}})

	if err := s.Trigger(t.Context(), "j"); !errors.Is(err, errBoom) {
		t.Fatalf("Trigger = %v, want errBoom", err)
	}
	if got := s.Status()[0].LastError; got != "boom" {
		t.Errorf("LastError = %q, want boom", got)
	}

	fail.Store(false)
	if err := s.Trigger(t.Context(), "j"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := s.Status()[0].LastError; got != "" {
		t.Errorf("LastError after success = %q, want empty", got)
	}
}

func TestTriggerWhileRunning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := New(testOptions(), Job{Name: "slow", Every: time.Hour, Run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}})

	done := make(chan error, 1)
	go func() { done <- s.Trigger(t.Context(), "slow") }()
	<-started

	if err := s.Trigger(t.Context(), "slow"); !errors.Is(err, ErrRunning) {
		t.Errorf("second Trigger = %v, want ErrRunning", err)
	}
	if !s.Status()[0].Running {
		t.Error("Running = false while job blocks")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	if s.Status()[0].Running {
		t.Error("Running = true after job returned")
	}
}

func TestPanicRecovered(t *testing.T) {
	var calls atomic.Int64
	s := New(testOptions(), Job{Name: "p", Every: time.Hour, Run: func(context.Context) error {
		calls.Add(1)
		panic("kaboom")
	}})

	for range 2 {
		err := s.Trigger(t.Context(), "p")
		if err == nil || !strings.Contains(err.Error(), "kaboom") {
			t.Fatalf("Trigger = %v, want panic error", err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 (job must not stay marked running)", got)
	}
	st := s.Status()[0]
	if !strings.Contains(st.LastError, "panicked") || st.Running {
		t.Errorf("status = %+v", st)
	}
}

func TestNilRun(t *testing.T) {
	s := New(testOptions(), Job{Name: "empty", Every: time.Hour})
	if err := s.Trigger(t.Context(), "empty"); err == nil {
		t.Fatal("Trigger with nil Run succeeded")
	}
}

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

func TestRunTicksAndStops(t *testing.T) {
	var ticked, manual atomic.Int64
	s := New(testOptions(),
		Job{Name: "fast", Every: time.Millisecond, Run: counter(&ticked)},
		Job{Name: "manual", Run: counter(&manual)},
	)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, "three scheduled runs", func() bool { return ticked.Load() >= 3 })

	if err := s.Trigger(t.Context(), "manual"); err != nil {
		t.Fatalf("Trigger(manual) while Run active: %v", err)
	}
	if got := manual.Load(); got != 1 {
		t.Errorf("manual ran %d times, want 1 (Every 0 is never scheduled)", got)
	}
	st := s.Status()
	if st[0].Name != "fast" || st[0].NextRun == nil {
		t.Errorf("fast status while running = %+v, want NextRun set", st[0])
	}
	if st[1].NextRun != nil {
		t.Errorf("manual NextRun = %v, want nil", st[1].NextRun)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if next := s.Status()[0].NextRun; next != nil {
		t.Errorf("NextRun after stop = %v, want nil", next)
	}
}

func TestRunCancelsJobContext(t *testing.T) {
	started := make(chan struct{})
	var sawCancel atomic.Bool
	s := New(testOptions(), Job{Name: "block", Every: time.Millisecond, Run: func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		sawCancel.Store(true)
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !sawCancel.Load() {
		t.Error("job context was not cancelled before Run returned")
	}
}

func TestJitterBounds(t *testing.T) {
	const every = time.Second
	tests := []struct {
		name   string
		jitter float64
		want   float64
	}{
		{"default", 0, 0.1},
		{"custom", 0.25, 0.25},
		{"disabled", -1, 0},
		{"capped", 3, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Options{Jitter: tt.jitter})
			if s.jitter != tt.want {
				t.Fatalf("jitter = %v, want %v", s.jitter, tt.want)
			}
			span := time.Duration(tt.want * float64(every))
			for range 1000 {
				if d := s.firstDelay(every); d < 0 || d > span {
					t.Fatalf("firstDelay = %v, want within [0, %v]", d, span)
				}
				if d := s.interval(every); d < every-span || d > every+span {
					t.Fatalf("interval = %v, want within %v ± %v", d, every, span)
				}
			}
		})
	}
}

func TestJobDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	s := New(Options{Logger: slog.New(slog.DiscardHandler), Telemetry: instrument.Providers{MeterProvider: mp}},
		Job{Name: "sweep", Run: func(context.Context) error { return nil }},
		Job{Name: "gc", Run: func(context.Context) error { return errors.New("disk full") }},
	)
	for _, name := range []string{"sweep", "sweep", "gc"} {
		_ = s.Trigger(context.Background(), name)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kazari.job.duration" || m.Unit != "s" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				name, _ := dp.Attributes.Value("kazari.job.name")
				errType, _ := dp.Attributes.Value("error.type")
				got[name.AsString()+"/"+errType.AsString()] = dp.Count
			}
		}
	}
	want := map[string]uint64{"sweep/": 2, "gc/_OTHER": 1}
	if !maps.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
}

func TestJobSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())
	s := New(Options{Logger: slog.New(slog.DiscardHandler), Telemetry: instrument.Providers{TracerProvider: tp}},
		Job{Name: "sweep", Run: func(context.Context) error { return nil }},
		Job{Name: "gc", Run: func(context.Context) error { return errors.New("disk full") }},
	)
	_ = s.Trigger(context.Background(), "sweep")
	ctx, parent := tp.Tracer("test").Start(context.Background(), "POST /v1/jobs/{name}/run")
	_ = s.Trigger(ctx, "gc")
	parent.End()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, sp := range rec.Ended() {
		byName[sp.Name()] = sp
	}
	if sp := byName["job sweep"]; sp == nil || sp.Parent().IsValid() || sp.Status().Code == codes.Error {
		t.Errorf("job sweep = %v, want a successful root span", sp)
	}
	gc := byName["job gc"]
	if gc == nil || gc.Parent().SpanID() != parent.SpanContext().SpanID() || gc.Status().Code != codes.Error {
		t.Fatalf("job gc = %v, want a failed child of the request", gc)
	}
	attrs := map[string]string{}
	for _, kv := range gc.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["kazari.job.name"] != "gc" || attrs["error.type"] != "internal" {
		t.Errorf("attributes = %v", attrs)
	}
}
