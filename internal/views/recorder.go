package views

import (
	"context"
	"fmt"
	"hash/maphash"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/views"

const resultKey = attribute.Key("kazari.views.result")

// Results of an offered request, the values of kazari.views.result.
const (
	resultCounted = "counted"
	// resultDropped is a view that found the pending counts full.
	resultDropped = "dropped"
)

// Defaults of the Recorder's bounds.
const (
	DefaultMaxPending  = 100_000
	DefaultMaxVisitors = 200_000
)

// RecorderOptions configures a Recorder. The zero value is valid.
type RecorderOptions struct {
	// Readers records which member opened a members page.
	Readers bool
	// MaxPending caps the counts held between flushes, keyed by artifact
	// and day, file or reader; a view that needs a new key past it is
	// dropped. Default DefaultMaxPending.
	MaxPending int
	// MaxVisitors caps the visitors told apart in a day; past it a view
	// still counts, but not as a new visitor. Default DefaultMaxVisitors.
	MaxVisitors int
	Now         func() time.Time // default time.Now
	Telemetry   instrument.Providers
}

type dayKey struct{ id, day int64 }

type pathKey struct {
	id, day int64
	path    string
}

type readerKey struct {
	id     int64
	handle string
}

type dayCount struct{ views, visitors, member, link int64 }

type readerCount struct {
	first, last time.Time
	views       int64
}

type visitorKey struct {
	id   int64
	hash uint64
}

// pending is the counts between two flushes.
type pending struct {
	days    map[dayKey]*dayCount
	paths   map[pathKey]int64
	readers map[readerKey]*readerCount
}

func newPending() pending {
	return pending{days: map[dayKey]*dayCount{}, paths: map[pathKey]int64{}, readers: map[readerKey]*readerCount{}}
}

func (p *pending) size() int { return len(p.days) + len(p.paths) + len(p.readers) }

// Recorder counts views in memory. Record never waits on anything but its
// own lock; Flush writes the counts to the Store.
type Recorder struct {
	store       Store
	readers     bool
	maxPending  int
	maxVisitors int
	now         func() time.Time
	requests    metric.Int64Counter
	results     map[string][]metric.AddOption

	// flushing serialises Flush, so a failed batch is put back before the
	// next one is taken.
	flushing sync.Mutex

	mu sync.Mutex
	p  pending
	// day is the UTC day, in unix nanoseconds, that seed and seen belong
	// to. Both are replaced when the day changes, so a visitor's hash means
	// nothing the next day.
	day  int64
	seed maphash.Seed
	seen map[visitorKey]struct{}
}

// NewRecorder returns a Recorder that flushes to s.
func NewRecorder(s Store, o RecorderOptions) *Recorder {
	if o.MaxPending <= 0 {
		o.MaxPending = DefaultMaxPending
	}
	if o.MaxVisitors <= 0 {
		o.MaxVisitors = DefaultMaxVisitors
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	results := map[string][]metric.AddOption{}
	for _, r := range []string{resultCounted, resultDropped, string(NotPage), string(Prefetch), string(Bot)} {
		results[r] = []metric.AddOption{metric.WithAttributes(resultKey.String(r))}
	}
	return &Recorder{
		store: s, readers: o.Readers, maxPending: o.MaxPending, maxVisitors: o.MaxVisitors, now: o.Now,
		requests: instrument.Handle(o.Telemetry.Meter(scope).Int64Counter("kazari.views.requests",
			metric.WithUnit("{request}"), metric.WithDescription("Requests for artifact pages by whether they counted as a view."))),
		results: results,
		p:       newPending(),
		seen:    map[visitorKey]struct{}{},
	}
}

// Record counts h if r, a request answered with an HTML file of an
// artifact, is a page view.
func (rec *Recorder) Record(r *http.Request, h Hit) {
	if reason := Classify(r); reason != Counted {
		rec.requests.Add(r.Context(), 1, rec.results[string(reason)]...)
		return
	}
	result := resultCounted
	if !rec.add(h) {
		result = resultDropped
	}
	rec.requests.Add(r.Context(), 1, rec.results[result]...)
}

// add counts h and reports whether it fitted within MaxPending.
func (rec *Recorder) add(h Hit) bool {
	now := rec.now()
	day := Day(now).UnixNano()
	reader := rec.readers && h.Access.Via == ViaMember && h.Access.Handle != ""

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if day != rec.day {
		rec.day = day
		rec.seed = maphash.MakeSeed()
		clear(rec.seen)
	}

	dk := dayKey{h.ArtifactID, day}
	pk := pathKey{h.ArtifactID, day, h.Path}
	rk := readerKey{h.ArtifactID, h.Access.Handle}
	dc := rec.p.days[dk]
	_, hasPath := rec.p.paths[pk]
	var rc *readerCount
	if reader {
		rc = rec.p.readers[rk]
	}
	added := 0
	if dc == nil {
		added++
	}
	if !hasPath {
		added++
	}
	if reader && rc == nil {
		added++
	}
	if rec.p.size()+added > rec.maxPending {
		return false
	}

	if dc == nil {
		dc = &dayCount{}
		rec.p.days[dk] = dc
	}
	dc.views++
	switch h.Access.Via {
	case ViaMember:
		dc.member++
	case ViaShareLink:
		dc.link++
	}
	if rec.newVisitor(h) {
		dc.visitors++
	}
	rec.p.paths[pk]++
	if reader {
		if rc == nil {
			rc = &readerCount{first: now}
			rec.p.readers[rk] = rc
		}
		rc.last = now
		rc.views++
	}
	return true
}

// newVisitor reports whether h is the first view of its artifact today by
// its IP and user agent. Called with mu held.
func (rec *Recorder) newVisitor(h Hit) bool {
	var hash maphash.Hash
	hash.SetSeed(rec.seed)
	_, _ = hash.WriteString(h.IP)
	_ = hash.WriteByte(0)
	_, _ = hash.WriteString(h.UA)
	k := visitorKey{h.ArtifactID, hash.Sum64()}
	if _, ok := rec.seen[k]; ok {
		return false
	}
	if len(rec.seen) >= rec.maxVisitors {
		return false
	}
	rec.seen[k] = struct{}{}
	return true
}

// Flush adds the counts held since the last Flush to the Store. When the
// Store fails they are kept for the next Flush, within MaxPending.
func (rec *Recorder) Flush(ctx context.Context) error {
	rec.flushing.Lock()
	defer rec.flushing.Unlock()

	rec.mu.Lock()
	p := rec.p
	rec.p = newPending()
	rec.mu.Unlock()
	if p.size() == 0 {
		return nil
	}
	if err := rec.store.AddViews(ctx, p.batch()); err != nil {
		rec.mu.Lock()
		rec.restore(p)
		rec.mu.Unlock()
		return fmt.Errorf("views: flush: %w", err)
	}
	return nil
}

// restore adds the counts of a failed flush back. Called with mu held.
func (rec *Recorder) restore(old pending) {
	for k, c := range old.days {
		cur := rec.p.days[k]
		if cur == nil {
			if rec.p.size() >= rec.maxPending {
				continue
			}
			rec.p.days[k] = c
			continue
		}
		cur.views += c.views
		cur.visitors += c.visitors
		cur.member += c.member
		cur.link += c.link
	}
	for k, n := range old.paths {
		if _, ok := rec.p.paths[k]; !ok && rec.p.size() >= rec.maxPending {
			continue
		}
		rec.p.paths[k] += n
	}
	for k, c := range old.readers {
		cur := rec.p.readers[k]
		if cur == nil {
			if rec.p.size() >= rec.maxPending {
				continue
			}
			rec.p.readers[k] = c
			continue
		}
		cur.first = c.first
		cur.views += c.views
	}
}

func (p pending) batch() Batch {
	var b Batch
	for k, c := range p.days {
		b.Days = append(b.Days, DayCount{ArtifactID: k.id, Day: time.Unix(0, k.day).UTC(),
			Views: c.views, Visitors: c.visitors, MemberViews: c.member, LinkViews: c.link})
	}
	for k, n := range p.paths {
		b.Paths = append(b.Paths, PathCount{ArtifactID: k.id, Day: time.Unix(0, k.day).UTC(), Path: k.path, Views: n})
	}
	for k, c := range p.readers {
		b.Readers = append(b.Readers, ReaderCount{ArtifactID: k.id, Handle: k.handle, FirstAt: c.first, LastAt: c.last, Views: c.views})
	}
	return b
}
