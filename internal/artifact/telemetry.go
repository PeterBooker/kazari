package artifact

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/artifact"

// Attribute keys. Slugs and versions go on spans only; metrics carry the
// bounded keys.
const (
	slugKey            = attribute.Key("kazari.artifact.slug")
	versionKey         = attribute.Key("kazari.artifact.version")
	expectedVersionKey = attribute.Key("kazari.artifact.expected_version")
	fileCountKey       = attribute.Key("kazari.artifact.file_count")
	sizeKey            = attribute.Key("kazari.artifact.size")
	editCountKey       = attribute.Key("kazari.edit.count")
	editKindKey        = attribute.Key("kazari.edit.kind")
	editMatchKey       = attribute.Key("kazari.edit.match")
	publishKindKey     = attribute.Key("kazari.publish.kind")
	auditActionKey     = attribute.Key("kazari.audit.action")
	shareOperationKey  = attribute.Key("kazari.share_link.operation")
)

// Publish kinds, how a version came to be published.
const (
	publishCreate   = "create"
	publishUpdate   = "update"
	publishEdit     = "edit"
	publishImport   = "import"
	publishRollback = "rollback"
)

type metrics struct {
	actions     metric.Int64Counter
	publishes   metric.Int64Counter
	publishSize metric.Int64Histogram
	edits       metric.Int64Counter
	editRetries metric.Int64Counter
	imports     metric.Int64Counter
	shareLinks  metric.Int64Counter
}

func newMetrics(m metric.Meter) metrics {
	counter := func(name, unit, desc string) metric.Int64Counter {
		return instrument.Handle(m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc)))
	}
	return metrics{
		actions:   counter("kazari.artifact.actions", "{action}", "Audited changes to artifacts by action."),
		publishes: counter("kazari.artifact.publishes", "{publish}", "Versions published, by how."),
		publishSize: instrument.Handle(m.Int64Histogram("kazari.artifact.publish.size",
			metric.WithUnit("By"), metric.WithDescription("Logical size of each new version."),
			metric.WithExplicitBucketBoundaries(256, 1<<10, 4<<10, 16<<10, 64<<10, 256<<10, 1<<20, 4<<20, 16<<20, 64<<20))),
		edits:       counter("kazari.artifact.edits", "{edit}", "Edits applied, by kind and by how their text matched."),
		editRetries: counter("kazari.artifact.edit.retries", "{retry}", "Edits applied again after a concurrent publish."),
		imports:     counter("kazari.artifact.imports", "{import}", "Page imports."),
		shareLinks:  counter("kazari.share_link.operations", "{operation}", "Share links created, revoked and opened."),
	}
}

// span starts a service span under a recording parent.
func (s *Service) span(ctx context.Context, name, slug string) (context.Context, trace.Span) {
	ctx, span := instrument.StartChild(ctx, s.tracer, name)
	if slug != "" && span.IsRecording() {
		span.SetAttributes(slugKey.String(slug))
	}
	return ctx, span
}

func endSpan(span trace.Span, err error) { instrument.End(span, err, CodeOf(err)) }

// versionAttrs describe the version a call made or published.
func versionAttrs(span trace.Span, v *Version) {
	if v != nil && span.IsRecording() {
		span.SetAttributes(versionKey.Int(v.Number), sizeKey.Int64(v.SizeBytes))
		if len(v.Files) > 0 {
			span.SetAttributes(fileCountKey.Int(len(v.Files)))
		}
	}
}

func surfaceAttr(ctx context.Context) attribute.KeyValue {
	return instrument.Surface.String(instrument.OneOf(instrument.SurfaceFrom(ctx),
		instrument.SurfaceAPI, instrument.SurfaceMCP, instrument.SurfaceAdmin, instrument.SurfaceScheduler))
}

// errorType is error.type of a failed call: Kazari's code, else internal.
func errorType(err error) attribute.KeyValue {
	code := CodeOf(err)
	if code == "" {
		code = instrument.CodeInternal
	}
	return semconv.ErrorTypeKey.String(code)
}

// published counts an attempt to publish a version of kind.
func (s *Service) published(ctx context.Context, kind string, v *Version, err error) {
	kindAttr := publishKindKey.String(kind)
	if err != nil {
		s.m.publishes.Add(ctx, 1, metric.WithAttributes(kindAttr, surfaceAttr(ctx), errorType(err)))
		return
	}
	s.m.publishes.Add(ctx, 1, metric.WithAttributes(kindAttr, surfaceAttr(ctx)))
	if v != nil && kind != publishRollback {
		s.m.publishSize.Record(ctx, v.SizeBytes, metric.WithAttributes(kindAttr))
	}
}

// edited counts each applied edit by kind and match.
func (s *Service) edited(ctx context.Context, out []EditOutcome) {
	for _, o := range out {
		kind := "text"
		if o.Match == MatchAnchor {
			kind = "anchor"
		}
		s.m.edits.Add(ctx, 1, metric.WithAttributes(editKindKey.String(kind),
			editMatchKey.String(instrument.OneOf(o.Match, MatchExact, MatchTrailingWhitespace, MatchIndentation, MatchAnchor))))
	}
}

func (s *Service) countOutcome(ctx context.Context, c metric.Int64Counter, err error, attrs ...attribute.KeyValue) {
	if err != nil {
		attrs = append(attrs, errorType(err))
	}
	c.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func editAttrs(span trace.Span, out []EditOutcome) {
	if len(out) > 0 && span.IsRecording() {
		matches := make([]string, len(out))
		for i, o := range out {
			matches[i] = o.Match
		}
		span.SetAttributes(editCountKey.Int(len(out)), editMatchKey.StringSlice(matches))
	}
}

func importAttrs(span trace.Span, r *ImportReport) {
	if r != nil && span.IsRecording() {
		span.SetAttributes(
			attribute.Int("kazari.import.fetched_count", r.Fetched),
			attribute.Int("kazari.import.skipped_count", len(r.Skipped)),
			attribute.Int("kazari.import.scripts_removed_count", r.ScriptsRemoved),
		)
	}
}

func sweepAttrs(span trace.Span, r SweepReport) {
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Int("kazari.sweep.soft_deleted_count", r.SoftDeleted),
			attribute.Int("kazari.sweep.purged_count", r.Purged),
			attribute.Int("kazari.sweep.freed_count", r.Freed),
		)
	}
}

type census struct {
	artifacts map[[2]string]int64
	versions  Totals
	blobs     Totals
}

// observeCensus reports the artifacts by status and visibility, the
// versions and the blobs from one cached read. Every status and visibility
// pair is reported, zero included, so a series drops to zero instead of
// disappearing.
func (s *Service) observeCensus(m metric.Meter) {
	gauge := func(name, unit, desc string) metric.Int64ObservableUpDownCounter {
		return instrument.Handle(m.Int64ObservableUpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(desc)))
	}
	count := gauge("kazari.artifact.count", "{artifact}", "Artifacts by status and visibility.")
	versions := gauge("kazari.artifact.version.count", "{version}", "Versions of every artifact.")
	versionBytes := gauge("kazari.artifact.version.usage", "By", "Logical size of every version.")
	blobs := gauge("kazari.blob.count", "{blob}", "Blobs stored.")
	blobBytes := gauge("kazari.blob.usage", "By", "Size of the blobs stored.")

	visibilities := []Visibility{Public, Unlisted, Members}
	pairs := map[[2]string]metric.ObserveOption{}
	for _, st := range Statuses {
		for _, v := range visibilities {
			pairs[[2]string{st, string(v)}] = metric.WithAttributes(
				attribute.String("kazari.artifact.status", st), attribute.String("kazari.artifact.visibility", string(v)))
		}
	}
	cache := instrument.NewCached(instrument.CensusTTL, instrument.CensusTimeout, s.opts.Now, s.census)
	_ = instrument.Handle(m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		c, err := cache.Get(ctx)
		if err != nil {
			return err
		}
		for key, opt := range pairs {
			o.ObserveInt64(count, c.artifacts[key], opt)
		}
		o.ObserveInt64(versions, c.versions.Count)
		o.ObserveInt64(versionBytes, c.versions.Bytes)
		o.ObserveInt64(blobs, c.blobs.Count)
		o.ObserveInt64(blobBytes, c.blobs.Bytes)
		return nil
	}, count, versions, versionBytes, blobs, blobBytes))
}

func (s *Service) census(ctx context.Context) (census, error) {
	counts, err := s.store.CountArtifacts(ctx)
	if err != nil {
		return census{}, err
	}
	c := census{artifacts: map[[2]string]int64{}}
	for _, sc := range counts {
		c.artifacts[[2]string{sc.Status, string(sc.Visibility)}] += sc.Count
	}
	if c.versions, err = s.store.VersionTotals(ctx); err != nil {
		return census{}, err
	}
	if c.blobs, err = s.store.BlobTotals(ctx); err != nil {
		return census{}, err
	}
	return c, nil
}
