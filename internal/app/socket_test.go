package app_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

func TestAdminSocket(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	ctx := context.Background()

	socket := in.app.Addrs().Socket
	if want := filepath.Join(cfg.DataDir, config.DefaultAdminSocket); socket != want {
		t.Fatalf("Socket = %q, want %q", socket, want)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != fs.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", info.Mode())
	}

	// No token at all over the socket.
	local := cli.NewClient("http://kazari", "", socketClient(socket))
	if h, err := local.Health(ctx); err != nil || h.Status != "ok" {
		t.Fatalf("health over socket: %+v, %v", h, err)
	}
	u, err := local.CreateUser(ctx, api.CreateUserRequest{Handle: "ann", Role: "admin"})
	if err != nil || u.Handle != "ann" || u.Role != "admin" {
		t.Fatalf("create user over socket: %+v, %v", u, err)
	}
	if _, err := local.IssueKey(ctx, "ann", api.IssueKeyRequest{Label: "local"}); err != nil {
		t.Fatalf("issue key over socket: %v", err)
	}
	audited, err := local.Audit(ctx, cli.AuditParams{})
	var events []api.AuditEvent
	if audited != nil {
		events = audited.Events
	}
	if err != nil || len(events) == 0 || events[0].Actor != "admin" {
		t.Errorf("audit over socket: %+v, %v; want events by actor admin", events, err)
	}

	// The public admin host still wants a credential.
	public := cli.NewClient("http://admin.localhost", "", client(in.app.Addrs().HTTP))
	if _, err := public.ListUsers(ctx); !cli.IsCode(err, "unauthorized") {
		t.Errorf("public admin host without a token: err = %v, want unauthorized", err)
	}

	// A stale socket file from an earlier run is replaced on the next start.
	in.stop()
	runErr := <-in.done
	in.done <- runErr // the cleanup reads it too
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	in2 := start(t, cfg)
	if _, err := cli.NewClient("http://kazari", "", socketClient(in2.app.Addrs().Socket)).Health(ctx); err != nil {
		t.Fatalf("health after replacing a stale socket file: %v", err)
	}
}

func TestAdminSocketDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Socket = ""
	in := start(t, cfg)
	if s := in.app.Addrs().Socket; s != "" {
		t.Fatalf("Socket = %q, want none", s)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, config.DefaultAdminSocket)); err == nil {
		t.Error("socket file exists although admin.socket is empty")
	}
}

// logRecorder keeps every record's message and attributes.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Clone())
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

func (l *logRecorder) find(msg string) (slog.Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func (l *logRecorder) all() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]slog.Record(nil), l.records...)
}

func attr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
		}
		return true
	})
	return v
}

// firstRun starts and stops a fresh instance and returns what it logged,
// what it wrote to stderr and where its socket was.
func firstRun(t *testing.T, cfg config.Config) (*logRecorder, string, string) {
	t.Helper()
	rec := &logRecorder{}
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := app.New(ctx, cfg, app.Options{Logger: slog.New(rec), Stderr: &stderr, Argon2: cheapArgon2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-a.Started()
	socket := a.Addrs().Socket
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return rec, stderr.String(), socket
}

// Without built-in accounts the first admin is made with two commands.
func TestFirstRunHint(t *testing.T) {
	cfg := testConfig(t)
	off := false
	cfg.Auth.Local.Enabled = &off
	rec, stderr, socket := firstRun(t, cfg)

	serving, ok := rec.find("serving")
	if !ok || attr(serving, "pages") != "http://<slug>.localhost/" || attr(serving, "admin") != "http://admin.localhost/" {
		t.Errorf("serving line = %v (found %v), want the page, api, mcp and admin URLs", serving, ok)
	}
	hint, ok := rec.find("no users yet")
	if !ok || hint.Level != slog.LevelWarn {
		t.Fatalf("no users yet: found %v, level %v", ok, hint.Level)
	}
	if next, how := attr(hint, "next"), attr(hint, "how"); !strings.Contains(next, "kazari admin users create") ||
		!strings.Contains(how, socket) {
		t.Errorf("hint next=%q how=%q", next, how)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing without built-in accounts", stderr)
	}
}

// With built-in accounts a fresh instance prints a setup link, straight to
// stderr: no log record, which may be exported, carries it.
func TestFirstRunSetupLink(t *testing.T) {
	rec, stderr, _ := firstRun(t, testConfig(t))
	m := regexp.MustCompile(`http://localhost/account/setup\?token=(kzl_[a-z2-7]+)`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr holds no setup link: %q", stderr)
	}
	if !strings.Contains(stderr, "kazari admin setup-link") {
		t.Errorf("stderr does not say how to get a fresh link: %q", stderr)
	}
	hint, ok := rec.find("no users yet")
	if !ok || !strings.Contains(attr(hint, "next"), "kazari admin setup-link") {
		t.Errorf("no users yet: found %v, next %q", ok, attr(hint, "next"))
	}
	for _, r := range rec.all() {
		line := r.Message
		r.Attrs(func(a slog.Attr) bool { line += " " + a.Value.String(); return true })
		if strings.Contains(line, m[1]) || strings.Contains(line, "kzl_") {
			t.Errorf("a log record carries the setup token: %s", line)
		}
	}
}

func TestListenSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.sock")
	var bound string
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		bound = address
		ln, err := new(net.ListenConfig).Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("at bind, Lstat(%q) err = %v, want nothing at the path yet", path, err)
		}
		if info, err := os.Stat(filepath.Dir(address)); err != nil {
			t.Error(err)
		} else if info.Mode().Perm() != 0o700 {
			t.Errorf("at bind, the directory of %q has mode %v, want 0700", address, info.Mode())
		}
		return ln, nil
	}
	ln, err := app.ListenSocket(t.Context(), path, listen)
	if err != nil {
		t.Fatalf("ListenSocket(%q) err = %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	if filepath.Dir(bound) == dir {
		t.Errorf("ListenSocket(%q) bound %q, want a directory of its own", path, bound)
	}
	if info, err := os.Lstat(path); err != nil {
		t.Error(err)
	} else if info.Mode().Type() != fs.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Errorf("Lstat(%q) mode = %v, want a socket with 0600", path, info.Mode())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("ReadDir(%q) = %v, %v; want the socket alone", dir, entries, err)
	}

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
		accepted <- err
	}()
	conn, err := new(net.Dialer).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("dial %q: %v", path, err)
	}
	conn.Close()
	if err := <-accepted; err != nil {
		t.Errorf("Accept on %q: %v", path, err)
	}
}

func TestListenSocketBindFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.sock")
	refused := errors.New("refused")
	listen := func(context.Context, string, string) (net.Listener, error) { return nil, refused }
	if _, err := app.ListenSocket(t.Context(), path, listen); !errors.Is(err, refused) {
		t.Errorf("ListenSocket(%q) err = %v, want %v", path, err, refused)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("ReadDir(%q) = %v, %v; want nothing left behind", dir, entries, err)
	}
}
