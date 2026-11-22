// Package update asks GitHub for the newest release of Kazari, the one
// request the server makes on its own account, so that the dashboard can say
// when the instance is behind.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/update"

// DefaultURL answers with the newest release that is neither a draft nor a
// pre-release.
const DefaultURL = "https://api.github.com/repos/PeterBooker/kazari/releases/latest"

const (
	timeout      = 15 * time.Second
	maxBodyBytes = 1 << 20
	// userAgent names the software and nothing else: no version, no host.
	userAgent = "kazari"
)

// Options configures a Checker.
type Options struct {
	// Current is the running version, such as 0.7.0. A build without a
	// release version ("", "dev", a pseudo-version) never asks.
	Current string
	URL     string // default DefaultURL
	// HTTPClient makes the request. The default times out after 15 s.
	HTTPClient *http.Client
	Now        func() time.Time // default time.Now
	Telemetry  instrument.Providers
}

// Result is the outcome of the last check that got an answer. The zero
// value means none has.
type Result struct {
	Latest    string // the newest release, without a leading v
	Newer     bool   // Latest is ahead of the running version
	CheckedAt time.Time
}

// Checker remembers the newest release it has seen. It is safe for
// concurrent use.
type Checker struct {
	current []int // nil when the build has no release version
	url     string
	client  *http.Client
	now     func() time.Time

	mu     sync.Mutex
	result Result
}

// New returns a Checker for the build opts.Current names.
func New(opts Options) *Checker {
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: timeout}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	current, _ := parse(opts.Current)
	c := &Checker{current: current, url: opts.URL, client: opts.HTTPClient, now: opts.Now}
	c.observe(opts.Telemetry.Meter(scope))
	return c
}

// observe reports whether a newer release is known: 1 or 0 once a check
// has got an answer, nothing before. The version itself stays off the
// metric, where it would be a new series with every release.
func (c *Checker) observe(m metric.Meter) {
	available := instrument.Handle(m.Int64ObservableGauge("kazari.update.available",
		metric.WithUnit("{release}"), metric.WithDescription("Whether a release newer than the running one is known: 1 or 0.")))
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		r := c.Result()
		if r.CheckedAt.IsZero() {
			return nil
		}
		var v int64
		if r.Newer {
			v = 1
		}
		o.ObserveInt64(available, v)
		return nil
	}, available))
}

// Enabled reports whether Check asks at all: only a build stamped with a
// release version can be compared with one.
func (c *Checker) Enabled() bool { return c.current != nil }

// Result returns the outcome of the last check that got an answer.
func (c *Checker) Result() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result
}

// release is the part of GitHub's release object the check reads.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Check asks for the newest release and remembers it. A build without a
// release version makes no request. On an error the previous Result stands,
// so a failed check never reports an update.
func (c *Checker) Check(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("update: ask for the newest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: ask for the newest release: %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&rel); err != nil {
		return fmt.Errorf("update: read the newest release: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		return errors.New("update: the newest release is a draft or a pre-release")
	}
	latest, ok := parse(rel.TagName)
	if !ok {
		return fmt.Errorf("update: the newest release has no release version: %q", truncate(rel.TagName))
	}
	c.mu.Lock()
	c.result = Result{Latest: format(latest), Newer: compare(latest, c.current) > 0, CheckedAt: c.now()}
	c.mu.Unlock()
	return nil
}

const maxComponents = 4

// parse reads a release version: one to four dot-separated numbers, with or
// without a leading v. Anything else, a pre-release or a pseudo-version
// included, is not one.
func parse(v string) ([]int, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) > maxComponents {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return nil, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func format(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// compare treats a missing component as 0, so 1.0 equals 1.0.0.
func compare(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func truncate(s string) string {
	const limit = 64
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
