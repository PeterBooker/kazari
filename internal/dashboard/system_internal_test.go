package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/update"
)

func TestSettingsGroups(t *testing.T) {
	groups := settingsGroups(map[string]any{
		"base_domain":   "art.test",
		"external_port": 0,
		"telemetry":     map[string]any{"otlp": map[string]any{"endpoint": "", "headers": map[string]any{}}},
		"zeta":          map[string]any{"later": true},
		"auth": map[string]any{
			"admin_token": "***",
			"oidc":        nil,
			"tokens":      map[string]any{"access_ttl": "1h"},
			"local":       map[string]any{"enabled": nil, "min_password_length": 15},
		},
		"serve":  map[string]any{"reserved_slugs": []any{"api", "mcp"}, "csp": "default-src 'self'"},
		"listen": map[string]any{"https": ":443", "proxy_mode": false},
		"alpha":  map[string]any{"list": []any{}},
	})
	var names []string
	rows := map[string]settingRow{}
	for _, g := range groups {
		names = append(names, g.Name)
		for i, r := range g.Rows {
			rows[r.Key] = r
			if i > 0 && g.Rows[i-1].Key > r.Key {
				t.Errorf("%s: %s comes before %s", g.Name, g.Rows[i-1].Key, r.Key)
			}
		}
	}
	// As the reference file orders them, then what it does not name.
	if got, want := strings.Join(names, " "), "general listen serve auth telemetry alpha zeta"; got != want {
		t.Errorf("groups = %s, want %s", got, want)
	}
	for key, want := range map[string]settingRow{
		"base_domain":                    {Key: "base_domain", Value: "art.test"},
		"external_port":                  {Key: "external_port", Value: "0"},
		"auth.admin_token":               {Key: "auth.admin_token", Value: "***", Secret: true},
		"auth.oidc":                      {Key: "auth.oidc", Unset: true},
		"auth.tokens.access_ttl":         {Key: "auth.tokens.access_ttl", Value: "1h"},
		"auth.local.enabled":             {Key: "auth.local.enabled", Unset: true},
		"auth.local.min_password_length": {Key: "auth.local.min_password_length", Value: "15"},
		"serve.reserved_slugs":           {Key: "serve.reserved_slugs", Value: "api, mcp"},
		"listen.proxy_mode":              {Key: "listen.proxy_mode", Value: "false"},
		"telemetry.otlp.endpoint":        {Key: "telemetry.otlp.endpoint", Unset: true},
		"telemetry.otlp.headers":         {Key: "telemetry.otlp.headers", Unset: true},
		"alpha.list":                     {Key: "alpha.list", Unset: true},
	} {
		if got := rows[key]; got != want {
			t.Errorf("row %s = %+v, want %+v", key, got, want)
		}
	}
}

func TestSystemDiagnosticsText(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	rep := &preflight.Report{At: at, Findings: []preflight.Finding{{Check: "dns", Status: preflight.Fail, Detail: "no wildcard record"}}}
	got := diagnostics(buildinfo.Info{Version: "0.8.0", Commit: "abc1234", Go: "go1.27.4"}, "acme", at, rep,
		settingsGroups(map[string]any{"base_domain": "art.test", "auth": map[string]any{"admin_token": "***"}}))
	for _, want := range []string{"kazari 0.8.0 (abc1234)\n", "go: go1.27.4\n", "tls: acme\n", "started: 2026-11-21T12:00:00Z\n",
		"checks (2026-11-21T12:00:00Z):\n", "dns", "fail", "no wildcard record", "  base_domain: art.test\n", "  auth.admin_token: ***\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, got)
		}
	}
	if got := diagnostics(buildinfo.Info{Version: "dev"}, "proxy", time.Time{}, nil, nil); !strings.Contains(got, "checks: not run yet") || strings.Contains(got, "settings") {
		t.Errorf("diagnostics without checks or settings:\n%s", got)
	}
}

func TestSystemUpdateView(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	if v := updateOf(update.Result{}); !v.Unknown || v.Newer {
		t.Errorf("no answer yet = %+v", v)
	}
	if v := updateOf(update.Result{Latest: "0.8.0", CheckedAt: at}); v.Unknown || v.Newer || !v.CheckedAt.Equal(at) {
		t.Errorf("up to date = %+v", v)
	}
	if v := updateOf(update.Result{Latest: "0.9.0", Newer: true, CheckedAt: at}); !v.Newer || v.Latest != "0.9.0" {
		t.Errorf("newer = %+v", v)
	}
}
