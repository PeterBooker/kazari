// Package scheduler runs named background jobs on jittered intervals and
// lets callers trigger them on demand.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/scheduler"

var ErrUnknownJob = errors.New("scheduler: unknown job")

var ErrRunning = errors.New("scheduler: job already running")

const defaultJitter = 0.1

// maxJitter keeps an interval from collapsing to zero.
const maxJitter = 0.5

// Job is a named unit of background work.
type Job struct {
	Name  string
	Every time.Duration // <= 0 runs only through Trigger
	// Run should return promptly when ctx is cancelled.
	Run func(ctx context.Context) error
}

// Options configures a Scheduler. The zero value is valid.
type Options struct {
	Logger *slog.Logger     // nil means slog.Default()
	Now    func() time.Time // stamps LastRun and NextRun only; nil means time.Now
	// Jitter is the fraction of Every by which intervals vary, and the cap
	// of the random delay before the first run. Zero means 0.1, negative
	// disables it, and it is capped at 0.5.
	Jitter    float64
	Telemetry instrument.Providers
}

// JobStatus is a snapshot of one job's state.
type JobStatus struct {
	Name      string
	Every     time.Duration
	LastRun   *time.Time // start of the most recent completed run; nil if never run
	LastError string     // "" on success
	NextRun   *time.Time // nil when not scheduled
	Running   bool
}

// Scheduler runs a fixed set of jobs, at most one run at a time per job.
type Scheduler struct {
	logger   *slog.Logger
	now      func() time.Time
	jitter   float64
	jobs     map[string]*jobState
	duration metric.Float64Histogram
	tracer   trace.Tracer
}

type jobState struct {
	job         Job
	spanName    string
	okAttrs     metric.MeasurementOption
	failedAttrs metric.MeasurementOption

	mu      sync.Mutex
	running bool
	lastRun *time.Time
	lastErr string
	nextRun *time.Time
}

// New returns a Scheduler for jobs; a later job replaces an earlier one of
// the same name.
func New(opts Options, jobs ...Job) *Scheduler {
	s := &Scheduler{
		logger: opts.Logger,
		now:    opts.Now,
		jitter: opts.Jitter,
		jobs:   make(map[string]*jobState, len(jobs)),
		duration: instrument.Handle(opts.Telemetry.Meter(scope).Float64Histogram("kazari.job.duration",
			metric.WithDescription("Duration of scheduler job runs."),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(0.01, 0.04, 0.16, 0.64, 2.56, 10.24, 40.96, 163.84))),
		tracer: opts.Telemetry.Tracer(scope),
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	switch {
	case s.jitter == 0:
		s.jitter = defaultJitter
	case s.jitter < 0:
		s.jitter = 0
	case s.jitter > maxJitter:
		s.jitter = maxJitter
	}
	for _, j := range jobs {
		name := instrument.JobName.String(j.Name)
		s.jobs[j.Name] = &jobState{
			job:         j,
			spanName:    "job " + j.Name,
			okAttrs:     metric.WithAttributeSet(attribute.NewSet(name)),
			failedAttrs: metric.WithAttributeSet(attribute.NewSet(name, semconv.ErrorTypeKey.String(instrument.Other))),
		}
	}
	return s
}

// Run schedules jobs with a positive Every until ctx is done: first after a
// random delay in [0, Jitter*Every], then every Every ± Jitter*Every. A run
// due while a Trigger holds the job is skipped. Run returns nil once every
// job goroutine, including any run in progress, has stopped.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, st := range s.jobs {
		if st.job.Every <= 0 {
			continue
		}
		wg.Go(func() { s.loop(ctx, st) })
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (s *Scheduler) loop(ctx context.Context, st *jobState) {
	defer st.setNext(nil)
	delay := s.firstDelay(st.job.Every)
	for {
		next := s.now().Add(delay)
		st.setNext(&next)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		err := s.run(ctx, st)
		if errors.Is(err, ErrRunning) {
			s.logger.Debug("scheduler: skipped run, job already running", "job", st.job.Name)
		}
		delay = s.interval(st.job.Every)
	}
}

// Trigger runs the named job synchronously and returns its error,
// ErrUnknownJob or ErrRunning. It works whether or not Run is active.
func (s *Scheduler) Trigger(ctx context.Context, name string) error {
	st, ok := s.jobs[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownJob, name)
	}
	return s.run(ctx, st)
}

// Status returns a snapshot of every job, sorted by name.
func (s *Scheduler) Status() []JobStatus {
	out := make([]JobStatus, 0, len(s.jobs))
	for _, st := range s.jobs {
		st.mu.Lock()
		out = append(out, JobStatus{
			Name:      st.job.Name,
			Every:     st.job.Every,
			LastRun:   copyTime(st.lastRun),
			LastError: st.lastErr,
			NextRun:   copyTime(st.nextRun),
			Running:   st.running,
		})
		st.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b JobStatus) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Scheduler) run(ctx context.Context, st *jobState) error {
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrRunning, st.job.Name)
	}
	st.running = true
	st.mu.Unlock()

	// A root span on schedule; a child of the request that triggered it
	// otherwise.
	ctx, span := s.tracer.Start(ctx, st.spanName, trace.WithAttributes(instrument.JobName.String(st.job.Name)))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := s.now()
	began := time.Now()
	err := safeRun(runCtx, st.job)
	instrument.End(span, err, "")
	attrs := st.okAttrs
	if err != nil {
		attrs = st.failedAttrs
	}
	s.duration.Record(ctx, time.Since(began).Seconds(), attrs)

	st.mu.Lock()
	st.running = false
	st.lastRun = &start
	st.lastErr = ""
	if err != nil {
		st.lastErr = err.Error()
	}
	st.mu.Unlock()

	if err != nil {
		s.logger.Error("scheduler: job failed", "job", st.job.Name, "err", err)
	}
	return err
}

func safeRun(ctx context.Context, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("scheduler: job %q panicked: %v", j.Name, r)
		}
	}()
	if j.Run == nil {
		return fmt.Errorf("scheduler: job %q has no Run func", j.Name)
	}
	return j.Run(ctx)
}

func (st *jobState) setNext(t *time.Time) {
	st.mu.Lock()
	st.nextRun = t
	st.mu.Unlock()
}

func (s *Scheduler) firstDelay(every time.Duration) time.Duration {
	span := time.Duration(s.jitter * float64(every))
	if span <= 0 {
		return 0
	}
	return rand.N(span + 1)
}

func (s *Scheduler) interval(every time.Duration) time.Duration {
	span := time.Duration(s.jitter * float64(every))
	if span <= 0 {
		return every
	}
	return every - span + rand.N(2*span+1)
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
