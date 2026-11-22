package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func checkOptions(endpoint string) Options {
	return Options{OTLP: OTLP{Endpoint: endpoint, Protocol: ProtocolHTTP},
		Traces: Traces{OTLP: true}, Metrics: Metrics{OTLP: true}, Logs: Logs{OTLP: true}}
}

func TestCheckSendsEachSignalOnce(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	t.Cleanup(srv.Close)

	results := Check(t.Context(), checkOptions(srv.URL))
	var signals []string
	for _, r := range results {
		signals = append(signals, r.Signal)
		if r.Err != nil || r.Problem != "" {
			t.Errorf("Check: %s = %v (%s), want it accepted", r.Signal, r.Err, r.Problem)
		}
		if want := srv.URL + "/v1/" + r.Signal; r.Endpoint != want || r.Protocol != ProtocolHTTP {
			t.Errorf("Check: %s went to %s over %s, want %s over %s", r.Signal, r.Endpoint, r.Protocol, want, ProtocolHTTP)
		}
	}
	if got, want := strings.Join(signals, " "), "traces metrics logs"; got != want {
		t.Errorf("Check reported %q, want %q", got, want)
	}
	if got, want := strings.Join(paths(recv.requests()), " "), "/v1/traces /v1/metrics /v1/logs"; got != want {
		t.Errorf("the collector received %q, want %q", got, want)
	}
}

// The problem is the advice kazari check prints, so each failure has to
// point at the setting that causes it.
func TestCheckNamesTheProblem(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{name: "credentials refused", status: http.StatusUnauthorized, want: "telemetry.otlp.headers"},
		{name: "credentials not allowed", status: http.StatusForbidden, want: "telemetry.otlp.headers"},
		{name: "no receiver at the path", status: http.StatusNotFound, want: "telemetry.otlp.endpoint and protocol"},
		{name: "another protocol at the path", status: http.StatusMethodNotAllowed, want: "telemetry.otlp.endpoint and protocol"},
		{name: "a collector that is failing", status: http.StatusServiceUnavailable, want: "the collector answered 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := &httpReceiver{status: tt.status}
			srv := httptest.NewServer(recv)
			t.Cleanup(srv.Close)

			results := Check(t.Context(), checkOptions(srv.URL))
			if len(results) != 3 {
				t.Fatalf("Check gave %d results, want one for each of the three signals", len(results))
			}
			for _, r := range results {
				if r.Err == nil || !strings.Contains(r.Problem, tt.want) {
					t.Errorf("Check: %s answered %d = %v with problem %q, want an error and a problem naming %q",
						r.Signal, tt.status, r.Err, r.Problem, tt.want)
				}
			}
			if got := len(recv.requests()); got != 3 {
				t.Errorf("the collector received %d requests, want 3: a check does not retry", got)
			}
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	o := checkOptions(srv.URL)
	o.Metrics, o.Logs = Metrics{}, Logs{}
	results := Check(t.Context(), o)
	if len(results) != 1 {
		t.Fatalf("Check gave %d results, want one for traces", len(results))
	}
	if r := results[0]; r.Err == nil || !strings.Contains(r.Problem, "could not reach the endpoint") {
		t.Errorf("Check of a closed port = %v with problem %q, want an error and a problem about reaching the endpoint", r.Err, r.Problem)
	}
}

func TestCheckBadSettings(t *testing.T) {
	tests := []struct {
		name string
		otlp OTLP
	}{
		{name: "an endpoint that is not a URL", otlp: OTLP{Endpoint: "collector:4318"}},
		{name: "an unknown protocol", otlp: OTLP{Endpoint: "http://127.0.0.1:4318", Protocol: "thrift"}},
		{name: "an unknown compression", otlp: OTLP{Endpoint: "http://127.0.0.1:4318", Compression: "zstd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := Check(t.Context(), Options{OTLP: tt.otlp, Traces: Traces{OTLP: true}})
			if len(results) != 1 {
				t.Fatalf("Check gave %d results, want one for traces", len(results))
			}
			if r := results[0]; r.Err == nil || r.Problem != "fix the endpoint settings" {
				t.Errorf("Check(%+v) = %v with problem %q, want an error and the settings named", tt.otlp, r.Err, r.Problem)
			}
		})
	}
}

func TestCheckLeavesOutDisabledSignals(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	t.Cleanup(srv.Close)

	o := checkOptions(srv.URL)
	o.Traces, o.Logs = Traces{}, Logs{}
	results := Check(t.Context(), o)
	if len(results) != 1 || results[0].Signal != "metrics" {
		t.Errorf("Check with only metrics on = %+v, want metrics alone", results)
	}
	if got, want := strings.Join(paths(recv.requests()), " "), "/v1/metrics"; got != want {
		t.Errorf("the collector received %q, want %q", got, want)
	}
}
