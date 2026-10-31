package mcpserver

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/mcpconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
)

const scope = "github.com/PeterBooker/kazari/internal/mcpserver"

// methods are the JSON-RPC methods counted by name: the specification's and
// the Skills extension's. Any other is _OTHER.
var methods = []string{
	"initialize", "ping", "notifications/initialized", "notifications/cancelled", "notifications/progress",
	"tools/list", "tools/call", "resources/list", "resources/templates/list", "resources/read",
	"resources/subscribe", "resources/unsubscribe", "prompts/list", "prompts/get",
	"completion/complete", "logging/setLevel", "notifications/roots/list_changed",
	"skills/list", "skills/get",
}

// callerErrors are the JSON-RPC errors that describe a bad request, not a
// fault of the server: counted by code, never as errors. -32002 is the
// resource-not-found code the SDK sends under MCPGODEBUG=customresnotfounderrcode=1.
var callerErrors = []int64{
	jsonrpc.CodeParseError, jsonrpc.CodeInvalidRequest, jsonrpc.CodeMethodNotFound,
	jsonrpc.CodeInvalidParams, -32002,
}

// serverErrors are the other codes the server answers with.
var serverErrors = []int64{jsonrpc.CodeInternalError, mcp.CodeHeaderMismatch}

// errTypeTool is error.type of a tool call that ran and failed, as the MCP
// conventions name it; kazari.error.code says why.
const errTypeTool = "tool_error"

var operationBuckets = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

var sizeBuckets = []float64{256, 1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20, 64 << 20}

// observer records each JSON-RPC method as the MCP semantic conventions
// describe: a server span, mcp.server.operation.duration, and for tool calls
// the sizes of what went in and came out. Tool arguments and results are
// never recorded, only their sizes.
type observer struct {
	tracer     trace.Tracer
	duration   metric.Float64Histogram
	argSize    metric.Int64Histogram
	resultSize metric.Int64Histogram
	tools      []string
	versions   []string
	enabled    bool
}

func newObserver(p instrument.Providers, tools []string) *observer {
	m := p.Meter(scope)
	return &observer{
		tracer: p.Tracer(scope),
		duration: instrument.Handle(mcpconv.NewServerOperationDuration(m,
			metric.WithExplicitBucketBoundaries(operationBuckets...))).Inst(),
		argSize: instrument.Handle(m.Int64Histogram("kazari.mcp.tool.arguments.size",
			metric.WithDescription("Size of a tool call's arguments, which shows what clients send inline instead of uploading."),
			metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(sizeBuckets...))),
		resultSize: instrument.Handle(m.Int64Histogram("kazari.mcp.tool.result.size",
			metric.WithDescription("Size of a tool call's text and embedded content: what it adds to the model's context."),
			metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(sizeBuckets...))),
		tools:    tools,
		versions: mcp.SupportedProtocolVersions(),
		enabled:  p.TracerProvider != nil || p.MeterProvider != nil,
	}
}

type protocolKey struct{}

// withProtocolVersion keeps the HTTP version for the middleware, which sees
// the request's context but not the request.
func withProtocolVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), protocolKey{}, middleware.ProtocolVersion(r))))
	})
}

// middleware observes the methods a server acting as p receives.
func (o *observer) middleware(p auth.Principal) mcp.Middleware {
	cred := instrument.OneOf(p.Source, auth.SourceAPIKey, auth.SourceOAuth)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			c := o.start(ctx, method, req, p, cred)
			res, err := next(c.ctx, method, req)
			o.end(c, res, err, time.Since(start))
			return res, err
		}
	}
}

type call struct {
	ctx   context.Context
	span  trace.Span
	tool  string
	attrs []attribute.KeyValue
	args  int
}

func (o *observer) start(ctx context.Context, method string, req mcp.Request, p auth.Principal, cred string) *call {
	c := &call{}
	var tool, uri string
	switch params := req.GetParams().(type) {
	case *mcp.CallToolParamsRaw:
		tool, c.args = params.Name, len(params.Arguments)
		c.tool = instrument.OneOf(tool, o.tools...)
	case *mcp.ReadResourceParams:
		uri = params.URI
	}
	name := instrument.OneOf(method, methods...)
	c.attrs = []attribute.KeyValue{
		semconv.McpMethodNameKey.String(name),
		semconv.NetworkTransportTCP,
		semconv.NetworkProtocolName("http"),
		instrument.CredentialType.String(cred),
	}
	if v, ok := ctx.Value(protocolKey{}).(string); ok {
		c.attrs = append(c.attrs, semconv.NetworkProtocolVersion(v))
	}
	if extra := req.GetExtra(); extra != nil {
		if v := extra.Header.Get("Mcp-Protocol-Version"); v != "" {
			c.attrs = append(c.attrs, semconv.McpProtocolVersionKey.String(instrument.OneOf(v, o.versions...)))
		}
	}
	spanName := name
	if c.tool != "" {
		c.attrs = append(c.attrs, semconv.GenAIToolName(c.tool), semconv.GenAIOperationNameExecuteTool)
		if c.tool != instrument.Other {
			spanName = name + " " + c.tool
		}
	}

	// The MCP conventions keep the MCP and transport contexts apart: a
	// traceparent in _meta is the parent, and the HTTP request a link.
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer)}
	if remote := parentFromMeta(req.GetParams()); remote.IsValid() {
		opts = append(opts, trace.WithLinks(trace.LinkFromContext(ctx)))
		ctx = trace.ContextWithRemoteSpanContext(ctx, remote)
	}
	c.ctx, c.span = o.tracer.Start(ctx, spanName, opts...)
	if c.span.IsRecording() {
		c.span.SetAttributes(c.attrs...)
		if tool != "" {
			c.span.SetAttributes(semconv.GenAIToolName(tool), attribute.Int("kazari.mcp.tool.arguments.size", c.args))
		}
		if uri != "" {
			c.span.SetAttributes(semconv.McpResourceURIKey.String(uri))
		}
		if p.ClientID != "" {
			c.span.SetAttributes(instrument.OAuthClientID.String(p.ClientID))
		}
	}
	return c
}

// parentFromMeta reads traceparent and tracestate from params._meta;
// baggage is ignored there as in headers.
func parentFromMeta(params mcp.Params) trace.SpanContext {
	// A method without params can carry a typed nil, whose GetMeta panics.
	if params == nil || reflect.ValueOf(params).IsNil() {
		return trace.SpanContext{}
	}
	meta := params.GetMeta()
	carrier := propagation.MapCarrier{}
	for _, k := range []string{"traceparent", "tracestate"} {
		if v, ok := meta[k].(string); ok {
			carrier[k] = v
		}
	}
	if len(carrier) == 0 {
		return trace.SpanContext{}
	}
	return trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
}

func (o *observer) end(c *call, res mcp.Result, err error, elapsed time.Duration) {
	attrs := c.attrs
	var errType, message string
	var rpcErr *jsonrpc.Error
	switch {
	case errors.As(err, &rpcErr):
		status := instrument.Other
		if slices.Contains(callerErrors, rpcErr.Code) || slices.Contains(serverErrors, rpcErr.Code) {
			status = strconv.FormatInt(rpcErr.Code, 10)
		}
		attrs = append(attrs, semconv.RPCResponseStatusCode(status))
		if !slices.Contains(callerErrors, rpcErr.Code) {
			errType, message = status, rpcErr.Message
		}
	case err != nil:
		status := strconv.FormatInt(jsonrpc.CodeInternalError, 10)
		attrs = append(attrs, semconv.RPCResponseStatusCode(status))
		errType = status
	}

	tr, _ := res.(*mcp.CallToolResult)
	var size int
	if tr != nil && err == nil {
		size = resultSize(tr)
		switch {
		case tr.InputRequests != nil:
			attrs = append(attrs, attribute.Bool("kazari.mcp.input_required", true))
			c.span.AddEvent("kazari.mcp.input_request",
				trace.WithAttributes(attribute.StringSlice("kazari.mcp.input_request.keys", slices.Sorted(maps.Keys(tr.InputRequests)))))
		case tr.IsError:
			errType = errTypeTool
			if code := toolErrorCode(tr.GetError()); code != "" {
				attrs = append(attrs, instrument.ErrorCode.String(code))
			}
		}
	}
	if errType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
	}

	o.duration.Record(c.ctx, elapsed.Seconds(), metric.WithAttributes(attrs...))
	if c.tool != "" {
		tool := metric.WithAttributes(semconv.GenAIToolName(c.tool))
		o.argSize.Record(c.ctx, int64(c.args), tool)
		if tr != nil {
			o.resultSize.Record(c.ctx, int64(size), tool)
		}
	}

	if c.span.IsRecording() {
		c.span.SetAttributes(attrs[len(c.attrs):]...)
		if tr != nil {
			c.span.SetAttributes(attribute.Int("kazari.mcp.tool.result.size", size))
		}
		if errType != "" {
			c.span.SetStatus(codes.Error, message)
		}
	}
	c.span.End()
}

// toolErrorCode is the Kazari code of a failed tool call; an error that is
// not Kazari's comes from the SDK's validation of the arguments.
func toolErrorCode(err error) string {
	var te *toolError
	if errors.As(err, &te) {
		return instrument.OneOf(te.e.Code, api.ErrorCodes...)
	}
	if err != nil {
		return api.CodeInvalidArgument
	}
	return ""
}

func resultSize(r *mcp.CallToolResult) int {
	n := 0
	for _, c := range r.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			n += len(c.Text)
		case *mcp.ImageContent:
			n += len(c.Data)
		case *mcp.AudioContent:
			n += len(c.Data)
		case *mcp.EmbeddedResource:
			if c.Resource != nil {
				n += len(c.Resource.Text) + len(c.Resource.Blob)
			}
		}
	}
	return n
}
