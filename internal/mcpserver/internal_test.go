package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/present"
)

func TestParseGrace(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", 0, false},
		{"72h", 72 * time.Hour, false},
		{"14d", 14 * 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"soon", 0, true},
		{"-1h", 0, true},
		{"0d", 0, true},
	}
	for _, c := range cases {
		got, err := parseGrace(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("parseGrace(%q) = %v, %v", c.in, got, err)
		}
	}
}

func TestParseFileURI(t *testing.T) {
	cases := []struct {
		uri, slug, path string
		ok              bool
	}{
		{"kazari://artifacts/demo/files/index.html", "demo", "index.html", true},
		{"kazari://artifacts/demo/files/assets/a%20b.png", "demo", "assets/a b.png", true},
		{"kazari://artifacts/demo/files/assets%2Fapp.js", "demo", "assets/app.js", true},
		{"kazari://artifacts/demo", "", "", false},
		{"kazari://artifacts/demo/files/", "", "", false},
		{"https://example.com/demo/files/x", "", "", false},
	}
	for _, c := range cases {
		slug, path, ok := parseFileURI(c.uri)
		if slug != c.slug || path != c.path || ok != c.ok {
			t.Errorf("parseFileURI(%q) = %q, %q, %v", c.uri, slug, path, ok)
		}
	}
}

func TestDescribeCSP(t *testing.T) {
	policy := "default-src 'none'; script-src 'self' https://cdn.example.com; frame-ancestors 'none'"
	got := describeCSP(policy, true)
	for _, s := range []string{"scripts from own files, https://cdn.example.com", "styles from nowhere",
		"frames from nowhere", "cannot be embedded", "add origins with metadata kazari.csp", "warnings name blocked origins"} {
		if !strings.Contains(got, s) {
			t.Errorf("describeCSP lacks %q: %s", s, got)
		}
	}
	if strings.Contains(got, "media") || strings.Contains(got, policy) {
		t.Errorf("describeCSP names media-src the policy does not set, or repeats the raw policy: %s", got)
	}
	if got := describeCSP(policy, false); strings.Contains(got, "add origins") || !strings.Contains(got, "does not take kazari.csp") {
		t.Errorf("describeCSP without overrides = %s", got)
	}
	if got := describeCSP("", true); !strings.Contains(got, "none is sent") {
		t.Errorf("empty CSP = %q", got)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{2 << 20: "2 MiB", 256 << 10: "256 KiB", 1500: "1500 bytes", 1 << 30: "1 GiB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}

// checkText refuses base64 sent as text for a binary file, and nothing else.
func TestCheckText(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAwS2OUAAAAABJRU5ErkJggg=="
	cases := []struct {
		name, path, content, enc string
		refused                  bool
	}{
		{"base64 image", "img/a.png", png, "", true},
		{"data URL", "img/a.png", "data:image/png;base64," + png, "", true},
		{"upper-case extension", "img/a.PNG", png, "", true},
		{"with encoding", "img/a.png", png, "base64", false},
		{"text file", "notes.txt", png, "", false},
		{"caption", "img/a.png", "a short caption", "", false},
		{"short word", "img/a.png", "caption text", "", false},
	}
	for _, c := range cases {
		err := checkText(c.path, c.content, c.enc)
		if refused := err != nil && strings.Contains(err.Error(), `add encoding: "base64"`); refused != c.refused || (err != nil && !refused) {
			t.Errorf("%s: checkText(%q, %.20q, %q) = %v, want refused %v", c.name, c.path, c.content, c.enc, err, c.refused)
		}
	}
}

func TestObserverBoundsMethods(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	o := newObserver(instrument.Providers{MeterProvider: mp}, []string{"get_artifact"})
	handler := o.middleware(auth.Principal{Source: auth.SourceAPIKey})(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "no such method"}
	})
	for i := range 50 {
		_, _ = handler(context.Background(), fmt.Sprintf("invented/method-%d", i), &mcp.ListToolsRequest{})
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mcp.server.operation.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				series++
				name, _ := dp.Attributes.Value("mcp.method.name")
				status, _ := dp.Attributes.Value("rpc.response.status_code")
				_, hasErr := dp.Attributes.Value("error.type")
				if name.AsString() != "_OTHER" || status.AsString() != "-32601" || hasErr || dp.Count != 50 {
					t.Errorf("point = %v count %d", dp.Attributes.ToSlice(), dp.Count)
				}
			}
		}
	}
	if series != 1 {
		t.Errorf("series = %d, want 1", series)
	}
}

func TestToolNames(t *testing.T) {
	for _, fetch := range []bool{true, false} {
		names := toolNames(newTexts(present.Links{URL: func(string) string { return "" }}, Options{Fetch: fetch}.withDefaults()), Options{Fetch: fetch})
		want := 19
		if !fetch {
			want = 18
		}
		if len(names) != want || slices.Contains(names, "import_page") != fetch {
			t.Errorf("fetch %v: %d names %v", fetch, len(names), names)
		}
	}
}
