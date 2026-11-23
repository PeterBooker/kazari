package cli_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/cli"
)

// lockedBuffer is written by the server's goroutines while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var startedHTTP = regexp.MustCompile(`"?http"?[=:]"?(127\.0\.0\.1:[0-9]+)`)

// startLocal runs kazari serve --local on a free port with its data in a
// temporary directory and returns that directory, the environment the
// client needs to find the socket, and the address it serves HTTP on.
func startLocal(t *testing.T) (string, map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	// The server reads the process environment, as it does under main.
	t.Setenv("KAZARI_DATA_DIR", dir)
	t.Setenv("KAZARI_LISTEN_HTTP", "127.0.0.1:0")
	env := map[string]string{"HOME": t.TempDir(), "KAZARI_DATA_DIR": dir}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"serve", "--local"}, cli.Env{Stdout: &stdout, Stderr: &stderr, LookupEnv: envMap(env)})
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
		m := startedHTTP.FindStringSubmatch(stderr.String())
		if info, err := os.Stat(socket); err == nil && info.Mode().Type() == fs.ModeSocket && m != nil {
			return dir, env, m[1]
		}
		select {
		case code := <-done:
			done <- code // for the cleanup
			t.Fatalf("serve exited %d before listening: %s", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no admin socket and HTTP address after 10s: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeLocalAdminSocket(t *testing.T) {
	dir, env, _ := startLocal(t)
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
	for _, want := range []string{"config       ok    --local, base domain kazari.localhost", "dns          skip", "dns token    skip  proxy mode", "ports        ok", "admin socket ok", "all checks passed"} {
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
	t.Chdir(t.TempDir())
	absent := filepath.Join(t.TempDir(), "kazari.yaml")
	r = runEnv(t, cli.Env{LookupEnv: envMap(map[string]string{"HOME": t.TempDir()}), DefaultConfig: absent}, "check")
	if r.code != 2 || !strings.Contains(r.stderr, "base_domain: required") ||
		!strings.Contains(r.stderr, "no configuration file: none was named with --config or $KAZARI_CONFIG, and "+absent+" does not exist") ||
		!strings.Contains(r.stderr, "run kazari check --local") {
		t.Errorf("unconfigured: %+v", r)
	}
	if strings.Contains(r.stderr, "or run kazari check") {
		t.Errorf("check sends the operator to itself: %s", r.stderr)
	}
	r = runCLI(t, map[string]string{"HOME": t.TempDir()}, "serve")
	if r.code != 2 || !strings.Contains(r.stderr, "or run kazari check") {
		t.Errorf("serve unconfigured: %+v, want a pointer to kazari check", r)
	}
	r = runCLI(t, nil, "serve", "--local", "--config", "x")
	if r.code != 2 || !strings.Contains(r.stderr, "--local") {
		t.Errorf("--local with --config: %+v", r)
	}
}

func TestCheckReportsSources(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kazari.yaml")
	if err := os.WriteFile(cfgPath, []byte("base_domain: kazari.localhost\nlisten:\n  proxy_mode: true\n  http: \"127.0.0.1:0\"\nexternal_scheme: http\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kazari.env"), []byte("KAZARI_DATA_DIR="+t.TempDir()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, nil, "check", "--config", cfgPath)
	if r.code != 0 {
		t.Fatalf("check: %+v", r)
	}
	for _, want := range []string{
		"config       ok    " + cfgPath + " with " + filepath.Join(dir, "kazari.env") + ", base domain kazari.localhost",
		"override     ok    data_dir from KAZARI_DATA_DIR in " + filepath.Join(dir, "kazari.env"),
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("check stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestBackupStreamAndRestore(t *testing.T) {
	dir, env, _ := startLocal(t)
	if r := runCLI(t, env, "admin", "users", "create", "ana", "--role", "admin"); r.code != 0 {
		t.Fatalf("create user: %+v", r)
	}
	r := runCLI(t, env, "admin", "backup", "-")
	if r.code != 0 || r.stdout == "" {
		t.Fatalf("admin backup -: %+v", r)
	}
	tr := tar.NewReader(strings.NewReader(r.stdout))
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	if !slices.Contains(names, "kazari.db") {
		t.Errorf("backup - archive = %v, want kazari.db in it", names)
	}

	backupDir := filepath.Join(t.TempDir(), "b")
	if r := runCLI(t, env, "admin", "backup", backupDir); r.code != 0 {
		t.Fatalf("admin backup: %+v", r)
	}
	// The running server holds its data directory.
	r = runCLI(t, env, "admin", "restore-backup", backupDir, "--data-dir", dir)
	if r.code != 2 || !strings.Contains(r.stderr, "another kazari (pid ") || !strings.Contains(r.stderr, "stop the server first") {
		t.Errorf("restore into a running server's directory: %+v", r)
	}

	target := filepath.Join(t.TempDir(), "data")
	r = runCLI(t, env, "admin", "restore-backup", backupDir, "--data-dir", target)
	if r.code != 0 || !strings.Contains(r.stdout, "into "+target) {
		t.Fatalf("restore into a new directory: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(target, "kazari.db")); err != nil {
		t.Errorf("restored kazari.db: %v", err)
	}
	r = runCLI(t, env, "admin", "restore-backup", backupDir, "--data-dir", target)
	if r.code != 2 || !strings.Contains(r.stderr, "add --force") {
		t.Errorf("restore over an instance without --force: %+v", r)
	}
	if r := runCLI(t, env, "admin", "restore-backup", backupDir, "--data-dir", target, "--force"); r.code != 0 {
		t.Errorf("restore with --force: %+v", r)
	}
	if r := runCLI(t, env, "admin", "restore-backup", t.TempDir(), "--data-dir", target, "--force"); r.code != 2 || !strings.Contains(r.stderr, "holds no kazari.db") {
		t.Errorf("restore of a directory that is no backup: %+v", r)
	}
}

func TestJournald(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stream"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	stream := fmt.Sprintf("%d:%d", st.Dev, st.Ino)

	if !cli.Journald(stream, f) {
		t.Error("stderr is the stream JOURNAL_STREAM names, but journald = false")
	}
	if cli.Journald(fmt.Sprintf("%d:%d", st.Dev, st.Ino+1), f) {
		t.Error("journald = true for another inode, as after a redirect")
	}
	if cli.Journald("", f) || cli.Journald(stream, &bytes.Buffer{}) {
		t.Error("journald = true without JOURNAL_STREAM or without a file")
	}
}
