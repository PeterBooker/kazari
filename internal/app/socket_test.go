package app_test

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	events, err := local.Audit(ctx, "", "", 0)
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

func TestFirstRunHint(t *testing.T) {
	cfg := testConfig(t)
	rec := &logRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := app.New(ctx, cfg, app.Options{Logger: slog.New(rec)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-a.Started()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	serving, ok := rec.find("serving")
	if !ok || attr(serving, "pages") != "http://<slug>.localhost/" || attr(serving, "admin") != "http://admin.localhost/" {
		t.Errorf("serving line = %v (found %v), want the page, api, mcp and admin URLs", serving, ok)
	}
	hint, ok := rec.find("no users yet")
	if !ok || hint.Level != slog.LevelWarn {
		t.Fatalf("no users yet: found %v, level %v", ok, hint.Level)
	}
	if next, how := attr(hint, "next"), attr(hint, "how"); !strings.Contains(next, "kazari admin users create") ||
		!strings.Contains(how, a.Addrs().Socket) {
		t.Errorf("hint next=%q how=%q", next, how)
	}
}
