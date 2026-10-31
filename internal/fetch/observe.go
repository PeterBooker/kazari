package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/fetch"

// Purposes, why the server fetched: the values of kazari.fetch.purpose.
const (
	PurposeFile   = "file"
	PurposeImport = "import"
	PurposeCIMD   = "cimd"
	PurposeOIDC   = "oidc"
)

var purposes = []string{PurposeFile, PurposeImport, PurposeCIMD, PurposeOIDC}

const purposeKey = attribute.Key("kazari.fetch.purpose")

type purposeCtxKey struct{}

// WithPurpose marks every request made with ctx as fetched for purpose.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeCtxKey{}, purpose)
}

func purposeOf(ctx context.Context) string {
	p, _ := ctx.Value(purposeCtxKey{}).(string)
	return instrument.OneOf(p, purposes...)
}

// Outcomes of a failed fetch besides an HTTP status: the values of
// error.type.
const (
	ErrRefused  = "refused"
	ErrTimeout  = "timeout"
	ErrTooLarge = "too_large"
)

// Observer records outbound requests: a client span for each attempt, and
// for each fetch, redirects included, its duration and size by purpose. Not
// http.client.request.duration, which requires server.address: fetched
// hosts are unbounded. One Observer serves every outbound client, so the
// metrics have one scope. A nil Observer records nothing.
type Observer struct {
	tracer   trace.Tracer
	duration metric.Float64Histogram
	size     metric.Int64Histogram
}

// NewObserver creates the instruments.
func NewObserver(p instrument.Providers) *Observer {
	m := p.Meter(scope)
	return &Observer{
		tracer: p.Tracer(scope),
		duration: instrument.Handle(m.Float64Histogram("kazari.fetch.duration",
			metric.WithDescription("Duration of a fetch for a member or sign-in, redirects included."),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(.05, .1, .25, .5, 1, 2.5, 5, 10, 15))),
		size: instrument.Handle(m.Int64Histogram("kazari.fetch.body.size",
			metric.WithDescription("Size of a fetched body."),
			metric.WithUnit("By"),
			metric.WithExplicitBucketBoundaries(256, 1<<10, 4<<10, 16<<10, 64<<10, 256<<10, 1<<20, 4<<20, 16<<20, 64<<20))),
	}
}

// Done records one fetch that started at start; errType is empty on
// success, else an HTTP status code or one of the Err* outcomes.
func (o *Observer) Done(ctx context.Context, start time.Time, size int64, errType string) {
	if o == nil {
		return
	}
	purpose := purposeKey.String(purposeOf(ctx))
	if errType != "" {
		o.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(purpose, semconv.ErrorTypeKey.String(errType)))
		return
	}
	o.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(purpose))
	o.size.Record(ctx, size, metric.WithAttributes(purpose))
}

// ErrorType classifies a transport error: the public-address check refused
// the dial, the request timed out, or _OTHER.
func ErrorType(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, errNonPublic):
		return ErrRefused
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ErrTimeout
	default:
		return instrument.Other
	}
}

// StatusErrorType is error.type of a response that did not succeed.
func StatusErrorType(status int) string {
	if status < 300 || status > 599 {
		return instrument.Other
	}
	return strconv.Itoa(status)
}

// Transport wraps next with a client span for each attempt. It injects no
// traceparent: fetched sites and identity providers are third parties.
func (o *Observer) Transport(next http.RoundTripper) http.RoundTripper {
	if o == nil {
		return next
	}
	return tracedTransport{o: o, next: next}
}

type tracedTransport struct {
	o    *Observer
	next http.RoundTripper
}

func (t tracedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := instrument.StartChild(r.Context(), t.o.tracer, r.Method, trace.WithSpanKind(trace.SpanKindClient))
	if !span.IsRecording() {
		return t.next.RoundTrip(r)
	}
	defer span.End()
	host, port := r.URL.Hostname(), r.URL.Port()
	portNum, err := strconv.Atoi(port)
	if err != nil {
		portNum = 80
		if r.URL.Scheme == "https" {
			portNum = 443
		}
	}
	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.ServerAddress(host),
		semconv.ServerPort(portNum),
		semconv.URLFull(redactURL(r.URL)),
		purposeKey.String(purposeOf(ctx)),
	)
	resends := 0
	for prev := r.Response; prev != nil && prev.Request != nil; prev = prev.Request.Response {
		resends++
	}
	if resends > 0 {
		span.SetAttributes(semconv.HTTPRequestResendCount(resends))
	}
	resp, err := t.next.RoundTrip(r.WithContext(ctx))
	if err != nil {
		span.SetAttributes(semconv.ErrorTypeKey.String(ErrorType(err)))
		span.SetStatus(codes.Error, "")
		return nil, err
	}
	span.SetAttributes(
		semconv.HTTPResponseStatusCode(resp.StatusCode),
		semconv.NetworkProtocolVersion(protocolVersion(resp)),
	)
	if resp.ContentLength >= 0 {
		span.SetAttributes(semconv.HTTPResponseBodySize(int(resp.ContentLength)))
	}
	if resp.StatusCode >= 400 {
		span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(resp.StatusCode)))
		span.SetStatus(codes.Error, "")
	}
	return resp, nil
}

// redactURL hides credentials and every query value, such as a presigned
// URL's signature.
func redactURL(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.UserPassword(instrument.Redacted, instrument.Redacted)
	}
	c.RawQuery = instrument.RedactQuery(c.RawQuery)
	c.Fragment, c.RawFragment = "", ""
	return c.String()
}

func protocolVersion(resp *http.Response) string {
	if resp.ProtoMajor == 1 {
		return "1." + strconv.Itoa(resp.ProtoMinor)
	}
	return strconv.Itoa(resp.ProtoMajor)
}
