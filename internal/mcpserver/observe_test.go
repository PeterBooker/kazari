package mcpserver_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/mcpserver"
	"github.com/PeterBooker/kazari/internal/middleware"
)

func observe(t *testing.T, e *env) (*sdkmetric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	e.opts.Telemetry = instrument.Providers{TracerProvider: tp, MeterProvider: mp}
	return reader, rec
}

type point struct {
	attrs map[string]string
	count uint64
	sum   float64
}

func points(t *testing.T, r *sdkmetric.ManualReader, name string) []point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []point
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{attrs: attrMap(dp.Attributes.ToSlice()), count: dp.Count, sum: dp.Sum})
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{attrs: attrMap(dp.Attributes.ToSlice()), count: dp.Count, sum: float64(dp.Sum)})
				}
			}
		}
	}
	return out
}

func find(pts []point, want map[string]string) (point, bool) {
	for _, p := range pts {
		match := true
		for k, v := range want {
			if p.attrs[k] != v {
				match = false
				break
			}
		}
		if match {
			return p, true
		}
	}
	return point{}, false
}

func spanNamed(rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestObserveToolCalls(t *testing.T) {
	e := newEnv(t)
	reader, rec := observe(t, e)
	cs := e.connect(t, member)
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>secret-body</h1>"}}})
	if res := call(t, cs, "get_artifact", map[string]any{"slug": "missing"}); !res.IsError {
		t.Fatal("get_artifact of a missing slug succeeded")
	}
	if res := call(t, cs, "get_artifact", map[string]any{"slug": 5}); !res.IsError {
		t.Fatal("get_artifact with a number for slug succeeded")
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "invented_tool"}); err == nil {
		t.Fatal("an unknown tool succeeded")
	}

	ops := points(t, reader, "mcp.server.operation.duration")
	cases := []struct {
		name  string
		match map[string]string
		want  map[string]string
		none  []string
	}{
		{"success", map[string]string{"gen_ai.tool.name": "create_artifact"},
			map[string]string{"mcp.method.name": "tools/call", "gen_ai.operation.name": "execute_tool", "kazari.credential.type": "api_key",
				"network.transport": "tcp", "network.protocol.name": "http"},
			[]string{"error.type", "rpc.response.status_code", "kazari.error.code"}},
		{"tool error", map[string]string{"gen_ai.tool.name": "get_artifact", "kazari.error.code": "not_found"},
			map[string]string{"error.type": "tool_error"}, []string{"rpc.response.status_code"}},
		{"schema", map[string]string{"gen_ai.tool.name": "get_artifact", "kazari.error.code": "invalid_argument"},
			map[string]string{"error.type": "tool_error"}, nil},
		{"unknown tool", map[string]string{"gen_ai.tool.name": "_OTHER"},
			map[string]string{"rpc.response.status_code": "-32602"}, []string{"error.type"}},
	}
	for _, c := range cases {
		p, found := find(ops, c.match)
		if !found {
			t.Errorf("%s: no point with %v in %v", c.name, c.match, ops)
			continue
		}
		for k, v := range c.want {
			if p.attrs[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.name, k, p.attrs[k], v)
			}
		}
		for _, k := range c.none {
			if _, ok := p.attrs[k]; ok {
				t.Errorf("%s: has %s", c.name, k)
			}
		}
	}
	for _, p := range ops {
		if p.attrs["gen_ai.tool.name"] == "invented_tool" {
			t.Error("an unknown tool's name became a series")
		}
	}

	if args, found := find(points(t, reader, "kazari.mcp.tool.arguments.size"), map[string]string{"gen_ai.tool.name": "create_artifact"}); !found || args.sum == 0 {
		t.Errorf("argument size = %+v", args)
	}
	if res, found := find(points(t, reader, "kazari.mcp.tool.result.size"), map[string]string{"gen_ai.tool.name": "create_artifact"}); !found || res.sum == 0 {
		t.Errorf("result size = %+v", res)
	}

	create := spanNamed(rec, "tools/call create_artifact")
	if create == nil {
		t.Fatalf("no create span in %v", spanNames(rec))
	}
	if failed := spanNamed(rec, "tools/call get_artifact"); failed == nil || failed.Status().Code != codes.Error {
		t.Errorf("get_artifact span = %v, want an error", failed)
	}
	unknown := spanNamed(rec, "tools/call")
	if unknown == nil || attrMap(unknown.Attributes())["gen_ai.tool.name"] != "invented_tool" || unknown.Status().Code == codes.Error {
		t.Errorf("unknown tool span = %v", unknown)
	}
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "secret-body") {
				t.Errorf("span %s carries argument text in %s", s.Name(), kv.Key)
			}
		}
	}
}

func TestObserveInputRequired(t *testing.T) {
	e := newEnv(t)
	reader, rec := observe(t, e)
	cs := e.connectWith(t, member, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}})
	ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo", "hard": true})

	if _, found := find(points(t, reader, "mcp.server.operation.duration"),
		map[string]string{"gen_ai.tool.name": "delete_artifact", "kazari.mcp.input_required": "true"}); !found {
		t.Error("no input_required point for delete_artifact")
	}
	asked := false
	for _, s := range rec.Ended() {
		for _, ev := range s.Events() {
			asked = asked || ev.Name == "kazari.mcp.input_request"
		}
	}
	if !asked {
		t.Error("no kazari.mcp.input_request event")
	}
}

// tracedHTTP serves the MCP handler behind Observe, as the app does.
func tracedHTTP(t *testing.T, e *env) *httptest.Server {
	t.Helper()
	e.deps.Auth = auth.AuthenticatorFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Handle: "alice", Scopes: auth.MemberScopes, Source: auth.SourceOAuth, ClientID: "https://claude.ai/client"}, nil
	})
	obs := middleware.NewObserver(e.opts.Telemetry, false)
	srv := httptest.NewServer(obs.Observe(instrument.SurfaceMCP, middleware.ObserveOptions{ControlPlane: true})(mcpserver.NewHandler(e.deps, e.opts)))
	t.Cleanup(srv.Close)
	return srv
}

// protocolVersion predates the per-request headers of 2026-07-28, which
// this hand-written client does not send.
const protocolVersion = "2025-11-25"

func rpc(t *testing.T, url, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestObserveParents(t *testing.T) {
	e := newEnv(t)
	reader, rec := observe(t, e)
	srv := tracedHTTP(t, e)
	rpc(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	rpc(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_artifacts","arguments":{},`+
		`"_meta":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}}`)

	list := spanNamed(rec, "tools/list")
	if list == nil || !list.Parent().IsValid() || list.Parent().IsRemote() {
		t.Fatalf("tools/list span = %v, want a child of the HTTP span; spans %v", list, spanNames(rec))
	}
	attrs := attrMap(list.Attributes())
	if attrs["kazari.oauth.client.id"] != "https://claude.ai/client" || attrs["kazari.credential.type"] != "oauth" ||
		attrs["mcp.protocol.version"] == "" || attrs["network.protocol.version"] != "1.1" {
		t.Errorf("tools/list attributes = %v", attrs)
	}

	called := spanNamed(rec, "tools/call list_artifacts")
	if called == nil {
		t.Fatalf("no tool span in %v", spanNames(rec))
	}
	if called.Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || len(called.Links()) != 1 {
		t.Errorf("tool span parent %v, links %v; want the _meta parent and a link to the HTTP span", called.Parent(), called.Links())
	}
	if _, found := find(points(t, reader, "mcp.server.operation.duration"),
		map[string]string{"mcp.method.name": "tools/call", "mcp.protocol.version": protocolVersion}); !found {
		t.Error("no tools/call point with the negotiated protocol version")
	}
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func spanNames(rec *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range rec.Ended() {
		out = append(out, s.Name())
	}
	return out
}
