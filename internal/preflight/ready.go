package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/bytesize"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/disk"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// Thresholds of Ready. A certificate this close to its end means renewal is
// failing: ACME renews a month ahead.
const (
	CertWarnWindow = 14 * 24 * time.Hour
	// CertGrace is how long after start a missing certificate is still
	// being obtained rather than a problem.
	CertGrace    = 5 * time.Minute
	DiskWarnFree = 0.10 // of the filesystem
	DiskFailFree = 0.02
)

// ReadyOptions are the inputs of Ready that a running server has and a
// configuration alone does not.
type ReadyOptions struct {
	// Options replace the network in tests; Telemetry is not used.
	Options
	// CertNotAfter is when the served certificate expires. Nil, or a zero
	// time, means none is served here, as in proxy mode.
	CertNotAfter func() time.Time
	// CertError is the error of the last failed certificate order, nil
	// when none failed since the last success; nil means none.
	CertError func() error
	// Started is when the server started, for CertGrace; zero means long ago.
	Started time.Time
	// DiskUsage reports the free bytes and the size of the filesystem
	// holding path; nil means disk.Usage.
	DiskUsage func(path string) (free, total uint64, err error)
	Now       func() time.Time // nil means time.Now
	// Providers receive what a Checker records; Ready itself records
	// nothing. (Options.Telemetry is the exporter's configuration, which
	// kazari check tests and a running server does not.)
	Providers instrument.Providers
}

// Ready checks a running server: the configuration, the DNS records, the
// DNS provider token, the certificate's expiry, the free space on the data
// directory's disk and the sizes of the database and the blob directory.
// Unlike Run it leaves the listener ports and the admin socket alone, which
// the server holds. The token check writes and removes a TXT record at the
// provider and may take up to Options.Timeout, so callers that run often go
// through a Checker.
func Ready(ctx context.Context, cfg config.Config, opts ReadyOptions) []Finding {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DiskUsage == nil {
		opts.DiskUsage = disk.Usage
	}
	var out []Finding
	add := func(check string, s Status, format string, args ...any) {
		out = append(out, Finding{Check: check, Status: s, Detail: fmt.Sprintf(format, args...)})
	}
	checkConfig(cfg, add)
	checkDNS(ctx, cfg, opts.Options, add)
	checkDNSToken(ctx, cfg, opts.Options, add)
	checkCertificate(cfg, opts, add)
	checkOIDC(ctx, cfg, opts.Options, add)
	checkDisk(cfg, opts, add)
	checkSizes(cfg, add)
	checkDashboard(cfg, add)
	return out
}

func checkConfig(cfg config.Config, add adder) {
	if err := cfg.Validate(); err != nil {
		add("config", Fail, "%s; correct the configuration and restart", strings.ReplaceAll(err.Error(), "\n", "; "))
		return
	}
	add("config", OK, "the configuration is valid")
}

func checkCertificate(cfg config.Config, opts ReadyOptions, add adder) {
	var notAfter time.Time
	if opts.CertNotAfter != nil {
		notAfter = opts.CertNotAfter()
	}
	if notAfter.IsZero() {
		var lastErr error
		if opts.CertError != nil {
			lastErr = opts.CertError()
		}
		since := opts.Now().Sub(opts.Started)
		switch {
		case cfg.Listen.ProxyMode:
			add("certificate", Skip, "proxy mode: the proxy holds the certificate")
		case lastErr != nil:
			add("certificate", Fail, "not obtained: %v; %s", lastErr, certs.ObtainHint(cfg.TLS.DNSProvider, lastErr))
		case opts.Started.IsZero() || since >= CertGrace:
			add("certificate", Warn, "no certificate yet; check the log for certificate errors (journalctl -u kazari | grep -i certificate)")
		default:
			add("certificate", Skip, "being obtained since the start %s ago", since.Round(time.Second))
		}
		return
	}
	next := "check the log for certificate errors and run kazari check"
	switch cfg.TLS.Mode {
	case "files":
		next = "replace tls.cert_file and tls.key_file; Kazari reloads them without a restart"
	case "self-signed":
		next = "restart Kazari to issue a new one"
	}
	left := notAfter.Sub(opts.Now())
	date := notAfter.UTC().Format("2006-01-02")
	switch {
	case left <= 0:
		add("certificate", Fail, "expired on %s; %s", date, next)
	case left < CertWarnWindow:
		add("certificate", Warn, "expires on %s, in %s; %s", date, days(left), next)
	default:
		add("certificate", OK, "valid until %s (%s)", date, days(left))
	}
}

func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	switch n {
	case 0:
		return "less than a day"
	case 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

func checkDisk(cfg config.Config, opts ReadyOptions, add adder) {
	free, total, err := opts.DiskUsage(cfg.DataDir)
	if errors.Is(err, disk.ErrUnsupported) {
		add("disk", Skip, "free space is not measured on this platform")
		return
	}
	if err == nil && total == 0 {
		err = errors.New("the filesystem reports no size")
	}
	if err != nil {
		add("disk", Warn, "free space on %s is unknown: %v; check that the data directory exists and is readable", cfg.DataDir, err)
		return
	}
	share := float64(free) / float64(total)
	detail := fmt.Sprintf("%s free of %s on the disk holding %s (%.0f%%)", bytesize.Format(int64(free)), bytesize.Format(int64(total)), cfg.DataDir, share*100)
	const next = "purge pages from the bin, lower lifecycle.retention or grow the disk"
	switch {
	case share < DiskFailFree:
		add("disk", Fail, "%s: publishing fails when it is full; %s", detail, next)
	case share < DiskWarnFree:
		add("disk", Warn, "%s; %s", detail, next)
	default:
		add("disk", OK, "%s", detail)
	}
}

func checkSizes(cfg config.Config, add adder) {
	var db int64
	for _, p := range []string{cfg.DBPath(), cfg.DBPath() + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			db += fi.Size()
		}
	}
	add("database", OK, "%s in %s", bytesize.Format(db), cfg.DBPath())
	blobs, err := disk.TreeSize(cfg.BlobDir())
	if err != nil {
		add("blobs", Warn, "%s cannot be read: %v; check its permissions", cfg.BlobDir(), err)
		return
	}
	add("blobs", OK, "%s in %s", bytesize.Format(blobs), cfg.BlobDir())
}

// Report is one run of Ready.
type Report struct {
	Findings []Finding
	At       time.Time // when the run finished
}

// Failed reports whether any finding is a Fail.
func (r Report) Failed() bool { return Failed(r.Findings) }

// DefaultReadyTTL is how long a Checker serves one run. The token check
// writes at the DNS provider, so it must not follow every page view.
const DefaultReadyTTL = 15 * time.Minute

// readyTimeout bounds one run: three lookups of ten seconds, the zone
// lookup and the token check of Options.Timeout, with room to spare.
const readyTimeout = 3 * time.Minute

// Checker runs Ready at most once per TTL however many callers ask, and one
// run at a time. It is safe for concurrent use.
type Checker struct {
	cfg  config.Config
	opts ReadyOptions
	ttl  time.Duration

	mu      sync.Mutex
	last    Report
	running chan struct{} // closed when the run in progress ends; nil when idle

	tracer trace.Tracer
}

const scope = "github.com/PeterBooker/kazari/internal/preflight"

// Attribute keys of the checker's telemetry. Both are bounded: the checks
// are a fixed list and a status is one of four.
const (
	checkKey  = attribute.Key("kazari.ready.check")
	statusKey = attribute.Key("kazari.ready.status")
)

// NewChecker returns a Checker for cfg. A ttl of 0 means DefaultReadyTTL.
func NewChecker(cfg config.Config, opts ReadyOptions, ttl time.Duration) *Checker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultReadyTTL
	}
	c := &Checker{cfg: cfg, opts: opts, ttl: ttl, tracer: opts.Providers.Tracer(scope)}
	c.observe(opts.Providers.Meter(scope))
	return c
}

// observe reports the findings of the last run by check and status, so an
// alert can fire on a failing check. Nothing runs for a scrape: until a run
// has finished there is nothing to report.
func (c *Checker) observe(m metric.Meter) {
	checks := instrument.Handle(m.Int64ObservableGauge("kazari.ready.checks",
		metric.WithUnit("{finding}"), metric.WithDescription("Findings of the last readiness run by check and status.")))
	at := instrument.Handle(m.Float64ObservableGauge("kazari.ready.timestamp",
		metric.WithUnit("s"), metric.WithDescription("When the readiness checks last ran, in seconds since the Unix epoch.")))
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		last, ok := c.Last()
		if !ok {
			return nil
		}
		counts := map[[2]string]int64{}
		for _, f := range last.Findings {
			counts[[2]string{f.Check, string(f.Status)}]++
		}
		for k, n := range counts {
			o.ObserveInt64(checks, n, metric.WithAttributes(checkKey.String(k[0]), statusKey.String(k[1])))
		}
		o.ObserveFloat64(at, float64(last.At.UnixNano())/1e9)
		return nil
	}, checks, at))
}

// Last returns the most recent report without running anything; ok is false
// before the first run has finished.
func (c *Checker) Last() (_ Report, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, !c.last.At.IsZero()
}

// Check returns a report no older than the TTL, running Ready when there is
// none; fresh asks for a new run whatever the age. Callers that arrive
// during a run wait for it and share its report. The run does not end with
// the caller that started it: when ctx is done first, Check returns its
// error and the run finishes for the next caller.
func (c *Checker) Check(ctx context.Context, fresh bool) (Report, error) {
	c.mu.Lock()
	if !fresh && !c.last.At.IsZero() && c.opts.Now().Sub(c.last.At) < c.ttl {
		defer c.mu.Unlock()
		return c.last, nil
	}
	done := c.running
	if done == nil {
		done = make(chan struct{})
		c.running = done
		go c.run(context.WithoutCancel(ctx), done)
	}
	c.mu.Unlock()

	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.last, nil
	case <-ctx.Done():
		return Report{}, ctx.Err()
	}
}

func (c *Checker) run(ctx context.Context, done chan struct{}) {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	// A child of the request that asked for the run, which it may outlive.
	ctx, span := instrument.StartChild(ctx, c.tracer, "preflight.ready")
	var findings []Finding
	defer func() {
		// No request's Recover middleware stands above this goroutine, and
		// the DNS provider's client is not Kazari's code.
		if p := recover(); p != nil {
			findings = []Finding{{Check: "ready", Status: Fail, Detail: fmt.Sprintf("the checks stopped with a panic: %v; report it with the log", p)}}
		}
		if span.IsRecording() {
			var failed, warned int
			for _, f := range findings {
				switch f.Status {
				case Fail:
					failed++
				case Warn:
					warned++
				}
			}
			span.SetAttributes(attribute.Int("kazari.ready.failed", failed), attribute.Int("kazari.ready.warned", warned))
		}
		// Before done closes: a caller that has its report also has the
		// run's span.
		span.End()
		c.mu.Lock()
		c.last = Report{Findings: findings, At: c.opts.Now()}
		c.running = nil
		c.mu.Unlock()
		close(done)
	}()
	findings = Ready(ctx, c.cfg, c.opts)
}
