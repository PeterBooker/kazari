package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestLoadEnvFileBesideConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kazari.yaml")
	if err := os.WriteFile(cfgPath, []byte(loadTestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	beside := writeEnvFileNamed(t, dir, EnvFileName, "KAZARI_TEST_DOMAIN=from.beside\nKAZARI_LISTEN_PROXY_MODE=true\nKAZARI_LISTEN_HTTP=127.0.0.1:8080\n")
	t.Chdir(t.TempDir())
	writeEnvFile(t, ".", "KAZARI_TEST_DOMAIN=from.dotenv\n")

	c, origin, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseDomain != "from.beside" || !c.Listen.ProxyMode {
		t.Errorf("Load = base %q, proxy mode %v; want from.beside and true from %s", c.BaseDomain, c.Listen.ProxyMode, beside)
	}
	if diff := cmp.Diff([]string{beside}, origin.EnvFiles); diff != "" {
		t.Errorf("EnvFiles (-want +got):\n%s", diff)
	}
	if got, want := origin.Describe("listen.proxy_mode"), "KAZARI_LISTEN_PROXY_MODE in "+beside; got != want {
		t.Errorf("Describe(listen.proxy_mode) = %q, want %q", got, want)
	}

	named := writeEnvFileNamed(t, t.TempDir(), "other.env", "KAZARI_TEST_DOMAIN=from.named\n")
	c, origin, err = Load(cfgPath, named)
	if err != nil || c.BaseDomain != "from.named" || c.Listen.ProxyMode {
		t.Errorf("Load with --env-file = base %q, proxy mode %v, %v; want from.named alone", c.BaseDomain, c.Listen.ProxyMode, err)
	}
	if diff := cmp.Diff([]string{named}, origin.EnvFiles); diff != "" {
		t.Errorf("EnvFiles with --env-file (-want +got):\n%s", diff)
	}

	t.Setenv("KAZARI_TEST_DOMAIN", "from.process")
	if c, _, err := Load(cfgPath, ""); err != nil || c.BaseDomain != "from.process" {
		t.Errorf("Load with the variable set = %q, %v; want from.process", c.BaseDomain, err)
	}
}

func TestLoadEnvFileBesideConfigUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0000")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kazari.yaml")
	if err := os.WriteFile(cfgPath, []byte("base_domain: art.kazari.test\ntls:\n  mode: self-signed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beside := writeEnvFileNamed(t, dir, EnvFileName, "KAZARI_LISTEN_PROXY_MODE=true\n")
	if err := os.Chmod(beside, 0); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	c, origin, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v, want the unreadable file skipped", err)
	}
	if c.Listen.ProxyMode || origin.Unreadable != beside+": permission denied" {
		t.Errorf("Load = proxy mode %v, Unreadable %q; want false and %s named", c.Listen.ProxyMode, origin.Unreadable, beside)
	}
}

func TestLoadReportsIgnoredOverrides(t *testing.T) {
	cfgPath := writeYAML(t, loadTestYAML)
	t.Chdir(t.TempDir())
	writeEnvFile(t, ".", "KAZARI_TEST_DOMAIN=from.dotenv\nKAZARI_LISTEN_PROXY_MODE=true\nOTHER=x\n")

	c, origin, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen.ProxyMode {
		t.Error("ProxyMode = true from a dotenv file nobody named")
	}
	if diff := cmp.Diff([]string{"KAZARI_LISTEN_PROXY_MODE"}, origin.Ignored); diff != "" {
		t.Errorf("Ignored (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{".env (references only)"}, origin.EnvFiles); diff != "" {
		t.Errorf("EnvFiles (-want +got):\n%s", diff)
	}
}

func TestOriginDescribe(t *testing.T) {
	cfgPath := writeYAML(t, "base_domain: art.kazari.test\nlisten:\n  http: \":80\"\ntls:\n  mode: self-signed\n")
	_, origin, err := loadOrigin(cfgPath, map[string]string{"KAZARI_LISTEN_HTTP": "127.0.0.1:8080", "KAZARI_LOG_LEVEL": "debug"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tests := []struct{ key, want string }{
		{"base_domain", "line 1 of " + cfgPath},
		{"listen.http", "KAZARI_LISTEN_HTTP in the environment, over line 3 of " + cfgPath},
		{"log.level", "KAZARI_LOG_LEVEL in the environment"},
		{"limits.max_files", "default"},
	}
	for _, tt := range tests {
		if got := origin.Describe(tt.key); got != tt.want {
			t.Errorf("Describe(%s) = %q, want %q", tt.key, got, tt.want)
		}
	}
	if diff := cmp.Diff([]string{"listen.http", "log.level"}, origin.Overrides()); diff != "" {
		t.Errorf("Overrides (-want +got):\n%s", diff)
	}
}

func loadOrigin(path string, vars map[string]string) (Config, Origin, error) {
	origin := Origin{File: path}
	cfg, err := load(Default(), path, env(vars), env(vars), nil, &origin)
	return cfg, origin, err
}

func TestEmptyVariableLeavesOIDCOff(t *testing.T) {
	base := map[string]string{"KAZARI_BASE_DOMAIN": "art.kazari.test", "KAZARI_TLS_MODE": "self-signed"}
	vars := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		maps.Copy(m, base)
		maps.Copy(m, extra)
		return m
	}
	c, err := LoadWith("", env(vars(map[string]string{"KAZARI_AUTH_OIDC_CLIENT_SECRET": "", "KAZARI_AUTH_OIDC_ADMIN_EMAILS": " "})))
	if err != nil {
		t.Fatalf("LoadWith with empty OIDC variables: %v", err)
	}
	if c.Auth.OIDC != nil || !c.LocalAccounts() {
		t.Errorf("OIDC = %+v, LocalAccounts = %v; want nil and true", c.Auth.OIDC, c.LocalAccounts())
	}

	_, err = LoadWith("", env(vars(map[string]string{"KAZARI_AUTH_OIDC_CLIENT_ID": "cid"})))
	want := "auth.oidc: on because of KAZARI_AUTH_OIDC_CLIENT_ID in the environment; remove that to sign in with built-in accounts"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("LoadWith with a lone client id = %v, want %q", err, want)
	}

	path := writeYAML(t, "base_domain: art.kazari.test\ntls:\n  mode: self-signed\nauth:\n  oidc:\n    client_id: cid\n")
	_, err = LoadWith(path, env(nil))
	want = "auth.oidc: on because of line 5 of " + path
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("LoadWith with auth.oidc in the file = %v, want %q", err, want)
	}
}

func TestLocalPortFollowsListener(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want int
	}{
		{"preset", nil, 8080},
		{"listener moved", map[string]string{"KAZARI_LISTEN_HTTP": "127.0.0.1:18080"}, 18080},
		{"both set", map[string]string{"KAZARI_LISTEN_HTTP": "127.0.0.1:18080", "KAZARI_EXTERNAL_PORT": "80"}, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := LoadLocalWith(env(tt.vars))
			if err != nil {
				t.Fatalf("LoadLocalWith: %v", err)
			}
			if c.ExternalPort != tt.want {
				t.Errorf("LoadLocalWith(%v).ExternalPort = %d, want %d", tt.vars, c.ExternalPort, tt.want)
			}
		})
	}
}

func TestExpandFromFile(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		doc  string
		vars map[string]string
		want string
	}{
		{"unset", "a: ${TOKEN}\n", map[string]string{"TOKEN_FILE": secret}, "from-file"},
		{"empty", "a: ${TOKEN}\n", map[string]string{"TOKEN": "", "TOKEN_FILE": secret}, "from-file"},
		{"set wins", "a: ${TOKEN}\n", map[string]string{"TOKEN": "direct", "TOKEN_FILE": secret}, "direct"},
		{"default", "a: ${TOKEN:-dflt}\n", map[string]string{"TOKEN_FILE": secret}, "from-file"},
		{"empty file variable", "a: ${TOKEN:-dflt}\n", map[string]string{"TOKEN_FILE": ""}, "dflt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandYAML(t, tt.doc, tt.vars)
			if err != nil {
				t.Fatalf("expand(%q): %v", tt.doc, err)
			}
			if got["a"] != tt.want {
				t.Errorf("expand(%q) = %v, want %q", tt.doc, got["a"], tt.want)
			}
		})
	}
	_, err := expandYAML(t, "a: ${TOKEN}\n", map[string]string{"TOKEN_FILE": filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "line 1: ${TOKEN}: TOKEN_FILE:") {
		t.Errorf("expand with a missing file = %v, want it named", err)
	}
}

func writeEnvFileNamed(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
