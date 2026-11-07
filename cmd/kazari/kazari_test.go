package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/cli"
)

// recorder is an API that keeps the Authorization header of every request.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (rec *recorder) seen() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.auth...)
}

// clearEnv leaves the process with none of the variables the client reads,
// so the only source left is the file a test writes.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KAZARI_URL", "KAZARI_TOKEN", "KAZARI_ADMIN_URL", "KAZARI_ADMIN_TOKEN", "KAZARI_ADMIN_SOCKET",
		"KAZARI_ENV_FILE", "KAZARI_CONFIG", "KAZARI_DATA_DIR", "XDG_CONFIG_HOME",
	} {
		t.Setenv(name, "") // restores the variable after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runClient(t *testing.T, args ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(args, strings.NewReader(""), &stdout, &stderr, clientEnv()); code != 0 {
		t.Logf("kazari %s exited %d: %s", strings.Join(args, " "), code, stderr.String())
	}
}

// A file in the working directory must not move a stored credential to
// another host, whichever setting it names.
func TestWorkingDirectoryDotenvKeepsCredentialDestination(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// config is the user's config file; the placeholder is the trusted URL.
		config string
		// dotenv is ./.env; the placeholder is the URL of the other host.
		dotenv string
		want   string
	}{
		{
			name:   "KAZARI_URL",
			args:   []string{"me"},
			config: "url: TRUSTED\ntoken: dummy-config-token\n",
			dotenv: "KAZARI_URL=OTHER\n",
			want:   "Bearer dummy-config-token",
		},
		{
			name:   "KAZARI_ADMIN_URL",
			args:   []string{"admin", "users", "list"},
			config: "admin_url: TRUSTED\nadmin_token: dummy-config-admin\n",
			dotenv: "KAZARI_ADMIN_URL=OTHER\n",
			want:   "Bearer dummy-config-admin",
		},
		{
			name:   "XDG_CONFIG_HOME",
			args:   []string{"me"},
			config: "url: TRUSTED\ntoken: dummy-config-token\n",
			dotenv: "XDG_CONFIG_HOME=OTHERCONFIG\n",
			want:   "Bearer dummy-config-token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			trusted, other := newRecorder(t), newRecorder(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeFile(t, filepath.Join(home, ".config", "kazari", "config.yaml"),
				strings.ReplaceAll(tt.config, "TRUSTED", trusted.URL))

			// A second config for the XDG case: the file names a config that
			// holds the other URL and keeps the stored token.
			otherConfig := t.TempDir()
			writeFile(t, filepath.Join(otherConfig, "kazari", "config.yaml"),
				"url: "+other.URL+"\ntoken: dummy-config-token\n")

			dir := t.TempDir()
			dotenv := strings.ReplaceAll(tt.dotenv, "OTHERCONFIG", otherConfig)
			writeFile(t, filepath.Join(dir, ".env"), strings.ReplaceAll(dotenv, "OTHER", other.URL))
			t.Chdir(dir)

			runClient(t, tt.args...)

			if got := other.seen(); len(got) != 0 {
				t.Errorf("the URL from ./.env received %q", got)
			}
			if got := trusted.seen(); len(got) != 1 || got[0] != tt.want {
				t.Errorf("the configured URL received %q, want one request with %q", got, tt.want)
			}
		})
	}
}

// Only the file $KAZARI_ENV_FILE names counts as the user's own, so it may
// hold a URL and a token, and ./.env beside it is still not read.
func TestNamedEnvFileSuppliesClientSettings(t *testing.T) {
	clearEnv(t)
	named, other := newRecorder(t), newRecorder(t)
	t.Setenv("HOME", t.TempDir())
	envFile := writeFile(t, filepath.Join(t.TempDir(), "client.env"),
		"KAZARI_URL="+named.URL+"\nKAZARI_TOKEN=dummy-file-token\n")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "KAZARI_URL="+other.URL+"\n")
	t.Chdir(dir)
	t.Setenv("KAZARI_ENV_FILE", envFile)

	runClient(t, "me")

	if got := other.seen(); len(got) != 0 {
		t.Errorf("the URL from ./.env received %q", got)
	}
	if got := named.seen(); len(got) != 1 || got[0] != "Bearer dummy-file-token" {
		t.Errorf("the URL from the named file received %q, want one request with the file's token", got)
	}
}

// serve and check resolve their config file, and the admin commands their
// socket, through the same lookup.
func TestWorkingDirectoryDotenvSetsNoLookupValue(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"),
		"KAZARI_CONFIG=/nonexistent/kazari.yaml\nKAZARI_ENV_FILE=/nonexistent/.env\nKAZARI_DATA_DIR=/nonexistent\nKAZARI_ADMIN_SOCKET=/nonexistent/admin.sock\n")
	t.Chdir(dir)

	lookup := clientEnv()
	for _, name := range []string{"KAZARI_CONFIG", "KAZARI_ENV_FILE", "KAZARI_DATA_DIR", "KAZARI_ADMIN_SOCKET"} {
		if v, ok := lookup(name); ok {
			t.Errorf("lookup(%s) = %q from ./.env, want unset", name, v)
		}
	}
}
