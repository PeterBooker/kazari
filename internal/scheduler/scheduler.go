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
)

// ErrUnknownJob is returned by Trigger for a name that was not registered.
var ErrUnknownJob = errors.New("scheduler: unknown job")

// ErrRunning is returned by Trigger when the job is already running.
var ErrRunning = errors.New("scheduler: job already running")

// defaultJitter is the jitter fraction used when Options.Jitter is zero.
const defaultJitter = 0.1

// maxJitter caps the jitter fraction so an interval never collapses to zero.
const maxJitter = 0.5

// Job is a named unit of background work.
type Job struct {
	// Name identifies the job for Trigger and Status. Names must be unique.
	Name string
	// Every is the interval between scheduled runs. A job with Every <= 0 is
	// never scheduled and runs only through Trigger.
	Every time.Duration
	// Run does the work. It should return promptly when ctx is cancelled.
	Run func(ctx context.Context) error
}

// Options configures a Scheduler. The zero value is valid.
type Options struct {
	// Logger receives job failures and panics. Nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock used to stamp LastRun and NextRun. Nil means time.Now.
	Now func() time.Time
	// Jitter is the fraction of Every by which each interval is randomly
	// shortened or lengthened, and the upper bound of the random delay
	// before the first run. Zero means 0.1, a negative value disables
	// jitter, and values above 0.5 are capped at 0.5.
	Jitter float64
}

// JobStatus is a snapshot of one job's state.
type JobStatus struct {
	Name      string
	Every     time.Duration
	LastRun   *time.Time // start of the most recent completed run; nil if never run
	LastError string     // error of the most recent completed run; "" on success
	NextRun   *time.Time // when the next scheduled run is due; nil if Run is not active
	Running   bool
}

// Scheduler runs a fixed set of jobs, at most one run at a time per job.
type Scheduler struct {
	logger *slog.Logger
	now    func() time.Time
	jitter float64
	jobs   map[string]*jobState
}

type jobState struct {
	job Job

	mu      sync.Mutex
	running bool
	lastRun *time.Time
	lastErr string
	nextRun *time.Time
}

// New returns a Scheduler for jobs. A later job with the same name as an
// earlier one replaces it.
func New(opts Options, jobs ...Job) *Scheduler {
	s := &Scheduler{
		logger: opts.Logger,
		now:    opts.Now,
		jitter: opts.Jitter,
		jobs:   make(map[string]*jobState, len(jobs)),
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
		s.jobs[j.Name] = &jobState{job: j}
	}
	return s
}

// Run schedules every job with a positive Every until ctx is done. The
// first run of each job happens after a random delay in [0, Jitter*Every],
// later runs every Every ± Jitter*Every. A scheduled run that finds the job
// already running (because of a Trigger) is skipped. Run returns nil once
// ctx is done and every job goroutine, including any run in progress, has
// stopped.
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

// Trigger runs the named job now, synchronously, and returns its error. It
// returns ErrUnknownJob for an unregistered name and ErrRunning when the job
// is already running. It works whether or not Run is active.
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

// run executes one run of st's job unless one is already in progress.
func (s *Scheduler) run(ctx context.Context, st *jobState) error {
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrRunning, st.job.Name)
	}
	st.running = true
	st.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := s.now()
	err := safeRun(runCtx, st.job)

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

// safeRun calls j.Run, turning a panic into an error.
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

// firstDelay is a random delay in [0, jitter*every].
func (s *Scheduler) firstDelay(every time.Duration) time.Duration {
	span := time.Duration(s.jitter * float64(every))
	if span <= 0 {
		return 0
	}
	return rand.N(span + 1)
}

// interval is every ± jitter*every, chosen uniformly.
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
