package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExpandValue(t *testing.T) {
	vars := env(map[string]string{"TOKEN": "s3cret", "EMPTY": "", "PORT": "8080"})
	tests := []struct {
		name, in, want string
	}{
		{"set", "${TOKEN}", "s3cret"},
		{"set but empty", "${EMPTY}", ""},
		{"default unused", "${TOKEN:-dflt}", "s3cret"},
		{"default for unset", "${MISSING:-dflt}", "dflt"},
		{"default for empty", "${EMPTY:-dflt}", "dflt"},
		{"empty default", "${MISSING:-}", ""},
		{"default with symbols", "${MISSING:-http://x:1/a?b=c$d{e # f}", "http://x:1/a?b=c$d{e # f"},
		{"several", "${MISSING:-127.0.0.1}:${PORT}", "127.0.0.1:8080"},
		{"dollar dollar", "$$5 and $${TOKEN}", "$5 and ${TOKEN}"},
		{"bare name untouched", "$TOKEN $ ${TOKEN}$", "$TOKEN $ s3cret$"},
		{"value not reexpanded", "${MISSING:-$${TOKEN}}", "$${TOKEN}"},
		{"no reference", "plain text", "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := expander{path: "kazari.yaml", lookup: vars, unset: map[string]bool{}}
			got, _ := e.expand(tt.in, 1)
			if len(e.errs) > 0 {
				t.Fatalf("expand(%q): %v", tt.in, e.errs)
			}
			if got != tt.want {
				t.Errorf("expand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func expandYAML(t *testing.T, doc string, vars map[string]string) (map[string]any, error) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatal(err)
	}
	if err := expandNode(&n, "kazari.yaml", env(vars)); err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(&n)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-encoded YAML does not parse: %v\n%s", err, out)
	}
	return m, nil
}

func TestExpandNode(t *testing.T) {
	vars := map[string]string{
		"N":      "8080",
		"B":      "true",
		"COLON":  "user: pass",
		"HASH":   "abc #def",
		"STAR":   "*not-an-alias",
		"AMP":    "&anchor",
		"DASH":   "- item",
		"NL":     "line1\nline2",
		"SPACES": "  padded  ",
		"QUOTES": `it's "quoted" \ yes`,
		"BRACE":  "{a: b}",
		"K":      "expanded-key",
	}
	tests := []struct {
		name, doc string
		want      map[string]any
	}{
		{"plain reference retyped", "a: ${N}\nb: ${B}\n", map[string]any{"a": 8080, "b": true}},
		{"quoted reference stays a string", "a: \"${N}\"\nb: '${B}'\n", map[string]any{"a": "8080", "b": "true"}},
		{"tagged reference stays a string", "a: !!str ${N}\n", map[string]any{"a": "8080"}},
		{"default retyped", "a: ${UNSET:-42}\n", map[string]any{"a": 42}},
		{"empty plain value is null", "a: ${UNSET:-}\n", map[string]any{"a": nil}},
		{"empty quoted value is a string", "a: \"${UNSET:-}\"\n", map[string]any{"a": ""}},
		{"colon space", "a: ${COLON}\n", map[string]any{"a": "user: pass"}},
		{"space hash", "a: ${HASH}\n", map[string]any{"a": "abc #def"}},
		{"leading star", "a: ${STAR}\nb: ${AMP}\nc: ${DASH}\n", map[string]any{"a": "*not-an-alias", "b": "&anchor", "c": "- item"}},
		{"newline", "a: ${NL}\nb: \"${NL}\"\n", map[string]any{"a": "line1\nline2", "b": "line1\nline2"}},
		{"surrounding spaces", "a: ${SPACES}\n", map[string]any{"a": "  padded  "}},
		{"quotes inside quotes", "a: \"${QUOTES}\"\nb: ${QUOTES}\n", map[string]any{"a": `it's "quoted" \ yes`, "b": `it's "quoted" \ yes`}},
		{"flow indicators", "a: [\"${BRACE}\", x]\n", map[string]any{"a": []any{"{a: b}", "x"}}},
		{"nested and lists", "a:\n  b:\n    - ${N}\n    - \"${N}\"\n", map[string]any{"a": map[string]any{"b": []any{8080, "8080"}}}},
		{"keys not expanded", "${K}: 1\n", map[string]any{"${K}": 1}},
		{"comments ignored", "# token: ${UNSET}\na: 1 # see ${ALSO_UNSET} $$\n", map[string]any{"a": 1}},
		{"alias expanded once", "a: &x $$${N}\nb: *x\n", map[string]any{"a": "$8080", "b": "$8080"}},
		{"literal block", "a: |\n  ${N}\n  $${N}\n", map[string]any{"a": "8080\n${N}\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandYAML(t, tt.doc, vars)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExpandErrors(t *testing.T) {
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"unset", "tls:\n  dns:\n    api_token: ${KAZARI_TLS_DNS_API_TOKEN}\n", []string{"config: kazari.yaml: ${KAZARI_TLS_DNS_API_TOKEN} is not set"}},
		{"every unset once", "a: ${A}\nb: ${B} ${A}\nc: [\"${A}\"]\n", []string{"config: kazari.yaml: ${A} is not set", "config: kazari.yaml: ${B} is not set"}},
		{"unterminated", "a: 1\nb: ${A\n", []string{`config: kazari.yaml: line 2: unterminated variable reference "${A"`}},
		{"empty name", "a: 1\nb: ${}\n", []string{"config: kazari.yaml: line 2: invalid variable reference ${}"}},
		{"bad name", "a: ${1A}\n", []string{"line 1: invalid variable reference ${1A}"}},
		{"other operator", "a: ${A-x}\n", []string{"invalid variable reference ${A-x}"}},
		{"space in name", "a: \"${ A }\"\n", []string{"invalid variable reference ${ A }"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandYAML(t, tt.doc, nil)
			if err == nil {
				t.Fatalf("expand(%q) = %v, want error", tt.doc, got)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if n := strings.Count(err.Error(), "\n") + 1; n != len(tt.want) {
				t.Errorf("error has %d lines, want %d: %q", n, len(tt.want), err)
			}
		})
	}
}

func TestIsEnvName(t *testing.T) {
	for _, s := range []string{"A", "_", "a_b", "KAZARI_TLS_DNS_API_TOKEN", "_1", "x9"} {
		if !isEnvName(s) {
			t.Errorf("isEnvName(%q) = false", s)
		}
	}
	for _, s := range []string{"", "1A", "A-B", "A B", "A.B", "É"} {
		if isEnvName(s) {
			t.Errorf("isEnvName(%q) = true", s)
		}
	}
}

func TestLoadExpands(t *testing.T) {
	path := writeYAML(t, `
# admin_token: ${NOT_SET_ANYWHERE}
base_domain: ${DOMAIN}
data_dir: ${DATA_DIR:-/var/lib/kazari}
external_port: ${PORT:-8080}
tls:
  mode: self-signed
  dns:
    api_token: "${DNS}"
limits:
  max_chunk_bytes: ${CHUNK:-1048576}
  upload_ttl: ${TTL:-2h}
serve:
  allow_csp_override: ${OVERRIDE:-false}
auth:
  admin_token: ${ADMIN}
  oidc:
    issuer: https://accounts.google.com
    client_id: ${CLIENT_ID}
    client_secret: ${SECRET}
`)
	vars := map[string]string{
		"DOMAIN":    "art.example.com",
		"DNS":       "12345",
		"ADMIN":     "*a: #b\nc-0123456789abcdef01234567",
		"CLIENT_ID": "0123",
		"SECRET":    "- [x] & ! % @ `",
	}
	c, err := LoadWith(path, env(vars))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"base_domain", c.BaseDomain, "art.example.com"},
		{"data_dir", c.DataDir, "/var/lib/kazari"},
		{"external_port", c.ExternalPort, 8080},
		{"tls.dns.api_token", c.TLS.DNS.APIToken, "12345"},
		{"limits.max_chunk_bytes", c.Limits.MaxChunkBytes, MiB},
		{"limits.upload_ttl", c.Limits.UploadTTL, Duration(2 * hour)},
		{"serve.allow_csp_override", c.Serve.AllowCSPOverride, false},
		{"auth.admin_token", c.Auth.AdminToken, "*a: #b\nc-0123456789abcdef01234567"},
		{"auth.oidc.client_id", c.Auth.OIDC.ClientID, "0123"},
		{"auth.oidc.client_secret", c.Auth.OIDC.ClientSecret, "- [x] & ! % @ `"},
		{"auth.oidc.auto_provision", c.Auth.OIDC.AutoProvision, true},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}

	_, err = LoadWith(path, env(map[string]string{"DNS": "x"}))
	for _, name := range []string{"DOMAIN", "ADMIN", "CLIENT_ID", "SECRET"} {
		if want := "config: " + path + ": ${" + name + "} is not set"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Errorf("err names a variable from a comment: %v", err)
	}
}
