package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startLocal runs kazari serve --local on a free port with its data in a
// temporary directory and returns that directory and the environment the
// client needs to find the socket.
func startLocal(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	// The server reads the process environment, as it does under main.
	t.Setenv("KAZARI_DATA_DIR", dir)
	t.Setenv("KAZARI_LISTEN_HTTP", "127.0.0.1:0")
	env := map[string]string{"HOME": t.TempDir(), "KAZARI_DATA_DIR": dir}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunContext(ctx, []string{"serve", "--local"}, strings.NewReader(""), &stdout, &stderr, envMap(env))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exited %d: %s", code, stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	})
	socket := filepath.Join(dir, "admin.sock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode().Type() == fs.ModeSocket {
			return dir, env
		}
		select {
		case code := <-done:
			done <- code // for the cleanup
			t.Fatalf("serve exited %d before listening: %s", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no admin socket after 10s: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeLocalAdminSocket(t *testing.T) {
	dir, env := startLocal(t)
	socket := filepath.Join(dir, "admin.sock")

	// Found from KAZARI_DATA_DIR: no URL, no token.
	r := runCLI(t, env, "admin", "health")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "ok (server ") {
		t.Fatalf("health over the discovered socket: %+v", r)
	}
	r = runCLI(t, env, "admin", "users", "create", "ana", "--role", "admin")
	if r.code != 0 || !strings.Contains(r.stdout, "handle:   ana\n") || !strings.Contains(r.stdout, "role:     admin\n") {
		t.Fatalf("create user over the discovered socket: %+v", r)
	}
	r = runCLI(t, env, "admin", "keys", "issue", "ana", "--label", "laptop")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "kzk_") {
		t.Fatalf("issue key over the discovered socket: %+v", r)
	}

	// Named explicitly, which wins over an admin URL that would not work.
	explicit := map[string]string{"HOME": env["HOME"], "KAZARI_ADMIN_URL": "http://127.0.0.1:1", "KAZARI_ADMIN_TOKEN": "x"}
	r = runCLI(t, explicit, "--admin-socket", socket, "admin", "users", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "ana") {
		t.Fatalf("users list over --admin-socket: %+v", r)
	}

	// Nothing found: the error says both ways in.
	r = runCLI(t, map[string]string{"HOME": env["HOME"]}, "admin", "users", "list")
	if r.code != 2 || !strings.Contains(r.stderr, "admin socket") || !strings.Contains(r.stderr, "KAZARI_ADMIN_URL") {
		t.Fatalf("without a socket or URL: %+v", r)
	}
}

func TestCheckLocal(t *testing.T) {
	t.Setenv("KAZARI_DATA_DIR", t.TempDir())
	t.Setenv("KAZARI_LISTEN_HTTP", "127.0.0.1:0")
	r := runCLI(t, nil, "check", "--local")
	if r.code != 0 {
		t.Fatalf("check --local: %+v", r)
	}
	for _, want := range []string{"config       ok    --local, base domain localhost", "dns          skip", "dns token    skip  proxy mode", "ports        ok", "admin socket ok", "all checks passed"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestServeConfigErrors(t *testing.T) {
	r := runCLI(t, map[string]string{"HOME": t.TempDir()}, "serve", "--config", filepath.Join(t.TempDir(), "nope.yaml"))
	if r.code != 2 || !strings.Contains(r.stderr, "nope.yaml") {
		t.Errorf("missing config file: %+v", r)
	}
	r = runCLI(t, map[string]string{"HOME": t.TempDir()}, "check")
	if r.code != 2 || !strings.Contains(r.stderr, "base_domain: required") || !strings.Contains(r.stderr, "kazari check") {
		t.Errorf("unconfigured: %+v", r)
	}
	r = runCLI(t, nil, "serve", "--local", "--config", "x")
	if r.code != 2 || !strings.Contains(r.stderr, "--local") {
		t.Errorf("--local with --config: %+v", r)
	}
}
