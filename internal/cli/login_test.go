package cli_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"

	"github.com/PeterBooker/kazari/internal/api"
)

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunLogin(t *testing.T) {
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/me": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer good-key" {
				reply(w, 401, api.ErrorResponse{Error: api.Error{Code: api.CodeUnauthorized, Message: "invalid key"}})
				return
			}
			reply(w, 200, api.Me{Handle: "peter", Role: "member"})
		},
	})
	home := t.TempDir()
	path := filepath.Join(home, ".config", "kazari", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("admin_socket: /run/kazari.sock\ntoken: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home}

	r := runCLIWith(t, env, strings.NewReader("bad-key\n"), nil, "login", "--url", srv.URL+"/v1")
	if r.code != exitUnauthorized || !strings.Contains(r.stderr, "kazari: unauthorized: invalid key") {
		t.Errorf("login with a bad key: %+v", r)
	}
	if got := readYAML(t, path)["token"]; got != "old" {
		t.Errorf("a failed login changed the token to %v", got)
	}

	r = runCLIWith(t, env, strings.NewReader("good-key\n"), nil, "login", "--url", srv.URL+"/v1")
	if want := "logged in to " + srv.URL + " as peter (member); saved in " + path + "\n"; r.code != 0 || r.stdout != want {
		t.Fatalf("login: %+v, want stdout %q", r, want)
	}
	want := map[string]any{"url": srv.URL, "token": "good-key", "admin_socket": "/run/kazari.sock"}
	if diff := cmp.Diff(want, readYAML(t, path)); diff != "" {
		t.Errorf("config after login (-want +got):\n%s", diff)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("config mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}

	if r = runCLI(t, env, "me"); r.code != 0 || !strings.Contains(r.stdout, "handle:") {
		t.Errorf("me after login: %+v", r)
	}

	r = runCLI(t, env, "logout")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "removed the key from "+path) {
		t.Errorf("logout: %+v", r)
	}
	if diff := cmp.Diff(map[string]any{"url": srv.URL, "admin_socket": "/run/kazari.sock"}, readYAML(t, path)); diff != "" {
		t.Errorf("config after logout (-want +got):\n%s", diff)
	}
	if r = runCLI(t, env, "me"); r.code != exitUsage || !strings.Contains(r.stderr, "no API token: run kazari login") {
		t.Errorf("me after logout: %+v", r)
	}
	if diff := cmp.Diff([]string{"Bearer bad-key", "Bearer good-key", "Bearer good-key"}, f.auth); diff != "" {
		t.Errorf("keys sent (-want +got):\n%s", diff)
	}
}

func TestRunLoginNeedsURL(t *testing.T) {
	r := runCLIWith(t, nil, strings.NewReader("k\n"), nil, "login")
	if r.code != exitUsage || !strings.Contains(r.stderr, "kazari login --url https://api.<base>") {
		t.Errorf("login without a URL: %+v", r)
	}
}

func TestConfigReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not say who may read a file on windows")
	}
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		wantErr bool
	}{
		{"private with a key", "token: k\n", 0o600, false},
		{"group-readable with a key", "token: k\n", 0o640, true},
		{"world-readable admin token", "admin_token: k\n", 0o604, true},
		{"world-readable without a key", "url: https://api.art.test\n", 0o644, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			r := runCLI(t, nil, "--client-config", path, "me")
			if got := strings.Contains(r.stderr, "run chmod 600 "+path); got != tt.wantErr {
				t.Errorf("me with %s: %+v; want an error naming chmod 600: %t", path, r, tt.wantErr)
			}
		})
	}
}
