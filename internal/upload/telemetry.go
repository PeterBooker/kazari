package upload

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/upload"

// idKey ties an upload's spans together across the requests it spans.
const idKey = attribute.Key("kazari.upload.id")

func (s *Service) span(ctx context.Context, name, id string) (context.Context, trace.Span) {
	ctx, span := instrument.StartChild(ctx, s.tracer, name)
	if id != "" && span.IsRecording() {
		span.SetAttributes(idKey.String(id))
	}
	return ctx, span
}

func endSpan(span trace.Span, err error) { instrument.End(span, err, artifact.CodeOf(err)) }

// chunk counts an appended chunk; a failure carries its code, chiefly
// upload_offset_mismatch from a client resending after a lost answer.
func (s *Service) chunk(ctx context.Context, err error) {
	if err == nil {
		s.chunks.Add(ctx, 1)
		return
	}
	code := artifact.CodeOf(err)
	if code == "" {
		code = instrument.CodeInternal
	}
	s.chunks.Add(ctx, 1, metric.WithAttributes(semconv.ErrorTypeKey.String(code)))
}

// observeOpen reports the open uploads, read from the store so the count is
// right after a restart.
func (s *Service) observeOpen(m metric.Meter) {
	count := instrument.Handle(m.Int64ObservableUpDownCounter("kazari.upload.count",
		metric.WithUnit("{upload}"), metric.WithDescription("Chunked uploads in progress.")))
	cache := instrument.NewCached(instrument.CensusTTL, instrument.CensusTimeout, s.opts.Now, s.store.CountAll)
	_ = instrument.Handle(m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		n, err := cache.Get(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(count, n)
		return nil
	}, count))
}
