package cli

import (
	"archive/tar"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

const (
	inviteURL = "https://art.test/account/invite?token=kzl_invitetoken"
	resetURL  = "https://art.test/account/reset?token=kzl_resettoken"
	setupURL  = "https://art.test/account/setup?token=kzl_setuptoken"
)

func TestRunAdminAccounts(t *testing.T) {
	expires := t0.Add(7 * 24 * time.Hour)
	active := t0.Add(time.Hour)
	three := 3
	var body, query string
	record := func(status int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body, query = strings.TrimSpace(string(b)), r.URL.RawQuery
			reply(w, status, v)
		}
	}
	invite := api.Link{ID: 7, User: "carol", Purpose: "invite", CreatedBy: "ann", CreatedAt: t0, ExpiresAt: expires}
	reset := api.Link{ID: 8, User: "bob", Purpose: "reset", RevokeAll: true, CreatedBy: "admin", CreatedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)}
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/users": record(200, []api.User{
			{Handle: "ann", Role: "admin", Password: true, Keys: 2, LastActiveAt: &active, CreatedAt: t0},
			{Handle: "bob", Role: "member", DisabledAt: &active, Invited: true, CreatedAt: t0},
			{Handle: "carol", Role: "member", Invited: true, CreatedAt: t0},
		}),
		"GET /v1/users/ann": record(200, api.User{Handle: "ann", Role: "admin", Password: true, Keys: 2, LastActiveAt: &active,
			Sessions: &three, CreatedAt: t0}),
		"GET /v1/users/carol": record(200, api.User{Handle: "carol", DisplayName: "Carol C", Role: "member", Invited: true, CreatedAt: t0}),
		"POST /v1/invites": record(201, api.Invited{User: api.User{Handle: "carol", Role: "member", Invited: true, CreatedAt: t0},
			Link: api.CreatedLink{Link: invite, URL: inviteURL}}),
		"POST /v1/users/carol/links":    record(201, api.CreatedLink{Link: invite, URL: inviteURL}),
		"POST /v1/users/bob/links":      record(201, api.CreatedLink{Link: reset, URL: resetURL}),
		"DELETE /v1/users/bob/sessions": record(204, nil),
		"GET /v1/invites":               record(200, []api.Link{reset, invite, {ID: 9, Purpose: "setup", CreatedBy: "admin", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}}),
		"DELETE /v1/invites/7":          record(204, nil),
		"POST /v1/setup-link": record(201, api.CreatedLink{URL: setupURL,
			ID: 9, Purpose: "setup", CreatedBy: "admin", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}),
	})
	tests := []struct {
		args       []string
		wantOut    string
		wantStderr string
		wantBody   string
	}{
		{args: []string{"users", "list"}, wantOut: "HANDLE  EMAIL  ROLE    STATE     USED  QUOTA  LAST ACTIVE                CREATED\n" +
			"ann     -      admin   active    0 B   -      2026-10-01T15:00:00+02:00  2026-10-01T14:00:00+02:00\n" +
			"bob     -      member  disabled  0 B   -      -                          2026-10-01T14:00:00+02:00\n" +
			"carol   -      member  invited   0 B   -      -                          2026-10-01T14:00:00+02:00\n"},
		{args: []string{"users", "show", "ann"}, wantOut: "handle:       ann\nemail:        -\nrole:         admin\nstorage:      0 B used (no quota)\n" +
			"password:     set\nkeys:         2\nlast active:  2026-10-01T15:00:00+02:00\nsessions:     3\ncreated:      2026-10-01T14:00:00+02:00\n"},
		{args: []string{"users", "show", "carol"}, wantOut: "handle:    carol\nname:      Carol C\nemail:     -\nrole:      member\nstorage:   0 B used (no quota)\n" +
			"password:  not set, invited\ncreated:   2026-10-01T14:00:00+02:00\n"},
		{args: []string{"users", "invite", "carol", "--email", "carol@x.test", "--name", "Carol C", "--role", "member", "--quota", "1GiB"},
			wantOut:    inviteURL + "\n",
			wantStderr: "invited carol as member: it works once, until 2026-10-08T14:00:00+02:00; send it over a channel you trust\n",
			wantBody:   `{"handle":"carol","email":"carol@x.test","display_name":"Carol C","role":"member","quota_bytes":1073741824}`},
		{args: []string{"users", "invite", "carol", "--replace"},
			wantOut:    inviteURL + "\n",
			wantStderr: "new invite link for carol: it works once, until 2026-10-08T14:00:00+02:00; send it over a channel you trust\n",
			wantBody:   `{"purpose":"invite"}`},
		{args: []string{"users", "reset-password", "bob", "--revoke-all"},
			wantOut: resetURL + "\n",
			wantStderr: "password reset link for bob, which also revokes their keys and connected apps: " +
				"it works once, until 2026-10-02T14:00:00+02:00; send it over a channel you trust\n",
			wantBody: `{"purpose":"reset","revoke_all":true}`},
		{args: []string{"users", "sign-out", "bob"}, wantOut: "signed bob out of every browser\n"},
		{args: []string{"invites", "list"}, wantOut: "ID  USER   PURPOSE                        BY     CREATED                    EXPIRES\n" +
			"8   bob    reset (revokes keys and apps)  admin  2026-10-01T14:00:00+02:00  2026-10-02T14:00:00+02:00\n" +
			"7   carol  invite                         ann    2026-10-01T14:00:00+02:00  2026-10-08T14:00:00+02:00\n" +
			"9   -      setup                          admin  2026-10-01T14:00:00+02:00  2026-10-01T15:00:00+02:00\n"},
		{args: []string{"invites", "revoke", "7"}, wantOut: "revoked link 7\n"},
		{args: []string{"setup-link"}, wantOut: setupURL + "\n",
			wantStderr: "setup link: whoever opens it becomes the first admin: it works once, until 2026-10-01T15:00:00+02:00; send it over a channel you trust\n"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			body, query = "", ""
			r := runCLI(t, nil, append([]string{"--admin-url", srv.URL, "--admin-token", "adm", "admin"}, tt.args...)...)
			if r.code != 0 {
				t.Fatalf("%+v", r)
			}
			if diff := cmp.Diff(tt.wantOut, r.stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantStderr, r.stderr); diff != "" {
				t.Errorf("stderr (-want +got):\n%s", diff)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s", body, tt.wantBody)
			}
			if query != "" {
				t.Errorf("query = %s, want none", query)
			}
		})
	}
	for _, a := range f.auth {
		if a != "Bearer adm" {
			t.Errorf("Authorization = %q", a)
		}
	}

	// --json prints the response, the link included, and nothing on stderr.
	r := runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "users", "invite", "carol")
	if r.code != 0 || r.stderr != "" || !strings.Contains(r.stdout, `"url": "`+inviteURL+`"`) || !strings.Contains(r.stdout, `"handle": "carol"`) {
		t.Errorf("invite --json: %+v", r)
	}
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "setup-link")
	if r.code != 0 || r.stderr != "" || !strings.Contains(r.stdout, `"url": "`+setupURL+`"`) {
		t.Errorf("setup-link --json: %+v", r)
	}

	seen := len(f.seen)
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "users", "invite", "carol", "--replace", "--role", "admin")
	if r.code != exitUsage || !strings.Contains(r.stderr, "--replace makes a link for an existing user") || len(f.seen) != seen {
		t.Errorf("--replace with --role: %+v, %d requests", r, len(f.seen)-seen)
	}
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "users", "invite", "carol", "--quota", "lots")
	if r.code != exitUsage || !strings.Contains(r.stderr, "--quota") || len(f.seen) != seen {
		t.Errorf("invite with a bad quota: %+v", r)
	}
}

func TestRunAdminLinkErrors(t *testing.T) {
	refuse := func(status int, code, msg string) http.HandlerFunc {
		return replyWith(status, api.ErrorResponse{Error: api.Error{Code: code, Message: msg}})
	}
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/setup-link":           refuse(409, api.CodeSetupDone, "the instance already has users; an admin invites the others"),
		"POST /v1/invites":              refuse(403, api.CodeForbidden, "built-in accounts are off on this instance"),
		"DELETE /v1/invites/7":          refuse(404, api.CodeNotFound, "link not found, or it was used, replaced or has expired"),
		"POST /v1/users/bob/links":      refuse(404, api.CodeNotFound, "user not found"),
		"DELETE /v1/users/bob/sessions": refuse(404, api.CodeNotFound, "user not found"),
	})
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"setup-link"}, "kazari: setup_done: the instance already has users"},
		{[]string{"users", "invite", "carol"}, "kazari: forbidden: built-in accounts are off"},
		{[]string{"invites", "revoke", "7"}, "kazari: not_found: link not found"},
		{[]string{"users", "reset-password", "bob"}, "kazari: not_found: user not found"},
		{[]string{"users", "sign-out", "bob"}, "kazari: not_found: user not found"},
	} {
		r := runCLI(t, nil, append([]string{"--admin-url", srv.URL, "--admin-token", "adm", "admin"}, tt.args...)...)
		if r.code != exitError || r.stdout != "" || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%v: %+v, want exit 1 and %q", tt.args, r, tt.want)
		}
	}
}

// archive is a backup as the server streams it.
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b strings.Builder
	tw := tar.NewWriter(&b)
	for _, name := range []string{"kazari.db", "blobs/ab/abcdef", "certmagic/key.pem"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}

func backupRoute(t *testing.T, query *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		*query = r.URL.RawQuery
		files := map[string]string{"kazari.db": "the database", "blobs/ab/abcdef": "a blob"}
		if r.URL.Query().Get("certs") == "true" {
			files["certmagic/key.pem"] = "the private key"
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(archive(t, files))
	}
}

func TestRunAdminBackup(t *testing.T) {
	var query string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{"POST /v1/backup": backupRoute(t, &query)})
	dir := filepath.Join(t.TempDir(), "backup")

	r := runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", dir)
	if r.code != 0 || r.stdout != "wrote 2 files, 18 B, to "+dir+"\n" {
		t.Fatalf("%+v", r)
	}
	// Over a URL the command never asks for the private key, and says so.
	if query != "" || !strings.Contains(r.stderr, "certmagic/ is not included") {
		t.Errorf("query = %q, stderr = %q", query, r.stderr)
	}
	for name, want := range map[string]string{"kazari.db": "the database", "blobs/ab/abcdef": "a blob"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}

	// A directory that holds anything is left alone, and so is a file; the
	// server is not asked, since a backup it started would be thrown away.
	query = "not asked"
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", dir)
	if r.code != exitUsage || !strings.Contains(r.stderr, "is not empty") || r.stdout != "" || query != "not asked" {
		t.Errorf("into a full directory: %+v, query %q", r, query)
	}
	file := filepath.Join(dir, "kazari.db")
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", file)
	if r.code != exitUsage || !strings.Contains(r.stderr, "is a file") || query != "not asked" {
		t.Errorf("onto a file: %+v, query %q", r, query)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "the database" {
		t.Errorf("the file the command was pointed at = %q, %v", got, err)
	}

	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "backup", filepath.Join(t.TempDir(), "b"))
	if r.code != 0 || !strings.Contains(r.stdout, `"files": 2`) || !strings.Contains(r.stdout, `"bytes": 18`) || !strings.Contains(r.stdout, `"certificates": false`) {
		t.Errorf("--json: %+v", r)
	}
}

func TestRunAdminBackupOverSocket(t *testing.T) {
	var (
		query   string
		proxied atomic.Bool // the server has no certmagic/ to send
	)
	withCerts := backupRoute(t, &query)
	f := &fakeAPI{t: t, routes: map[string]http.HandlerFunc{"POST /v1/backup": func(w http.ResponseWriter, r *http.Request) {
		if !proxied.Load() {
			withCerts(w, r)
			return
		}
		query = r.URL.RawQuery
		w.Write(archive(t, map[string]string{"kazari.db": "the database"}))
	}}}
	sockDir, err := os.MkdirTemp("", "kz") //nolint:usetesting // a Unix socket's path is bounded at about a hundred bytes, and t.TempDir's holds the test's name
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "admin.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	dir := filepath.Join(t.TempDir(), "backup")
	r := runCLI(t, nil, "--admin-socket", socket, "admin", "backup", dir)
	if r.code != 0 || r.stderr != "" || r.stdout != "wrote 3 files, 33 B, to "+dir+"\n" || query != "certs=true" {
		t.Fatalf("%+v, query %q", r, query)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "certmagic", "key.pem")); err != nil || string(got) != "the private key" {
		t.Errorf("certmagic/key.pem = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "kazari.db")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kazari.db mode = %v, %v; want 0600", info.Mode(), err)
	}

	dir = filepath.Join(t.TempDir(), "backup")
	r = runCLI(t, nil, "--admin-socket", socket, "admin", "backup", "--no-certs", dir)
	if r.code != 0 || query != "" || r.stdout != "wrote 2 files, 18 B, to "+dir+"\n" {
		t.Errorf("--no-certs: %+v, query %q", r, query)
	}

	// --json says whether the archive held the certificates, which a
	// server behind a proxy has none of to give.
	r = runCLI(t, nil, "--admin-socket", socket, "--json", "admin", "backup", filepath.Join(t.TempDir(), "backup"))
	if r.code != 0 || query != "certs=true" || !strings.Contains(r.stdout, `"certificates": true`) {
		t.Errorf("--json over the socket: %+v, query %q", r, query)
	}
	proxied.Store(true)
	r = runCLI(t, nil, "--admin-socket", socket, "--json", "admin", "backup", filepath.Join(t.TempDir(), "backup"))
	if r.code != 0 || query != "certs=true" || !strings.Contains(r.stdout, `"certificates": false`) {
		t.Errorf("--json over the socket of a server without certificates: %+v, query %q", r, query)
	}
}

func TestRunAdminBackupErrors(t *testing.T) {
	status, code := 409, api.CodeBackupRunning
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/backup": func(w http.ResponseWriter, r *http.Request) {
			if status == 200 {
				// Cut short, as a backup that fails on the server is.
				w.Write(archive(t, map[string]string{"kazari.db": strings.Repeat("x", 4096)})[:1024])
				return
			}
			reply(w, status, api.ErrorResponse{Error: api.Error{Code: code, Message: "refused"}})
		},
	})
	dir := filepath.Join(t.TempDir(), "backup")
	r := runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", dir)
	if r.code != exitError || !strings.Contains(r.stderr, "kazari: backup_running: refused") {
		t.Errorf("a running backup: %+v", r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a refused backup created %s: %v", dir, err)
	}
	status, code = 507, api.CodeNoSpace
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", dir)
	if r.code != exitError || !strings.Contains(r.stderr, "kazari: no_space: refused") {
		t.Errorf("no space: %+v", r)
	}
	status = 200
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "backup", dir)
	if r.code != exitError || !strings.Contains(r.stderr, "the backup is incomplete") || r.stdout != "" {
		t.Errorf("a truncated archive: %+v", r)
	}
}

func TestRunAdminReady(t *testing.T) {
	out := api.Ready{Ready: true, CheckedAt: t0, Checks: []api.Check{
		{Check: "config", Status: "ok", Detail: "valid"},
		{Check: "disk", Status: "warn", Detail: "8% free on /data; free some space"},
	}}
	var query string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/ready": func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			reply(w, 200, out)
		},
	})
	r := runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "ready")
	want := "config       ok    valid\ndisk         warn  8% free on /data; free some space\n\nall checks passed, 1 warning(s)\nchecked 2026-10-01T14:00:00+02:00\n"
	if r.code != 0 || r.stderr != "" || query != "" {
		t.Fatalf("%+v, query %q", r, query)
	}
	if diff := cmp.Diff(want, r.stdout); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	// A failed check prints like the others and fails the command.
	out.Ready = false
	out.Checks = append(out.Checks, api.Check{Check: "dns", Status: "fail", Detail: "*.art.test does not resolve; add the wildcard record"})
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "ready", "--fresh")
	if r.code != exitError || query != "fresh=true" || !strings.Contains(r.stdout, "dns          fail  *.art.test does not resolve; add the wildcard record\n\n1 check(s) failed\n") ||
		r.stderr != "kazari: readiness checks failed\n" {
		t.Errorf("with a failing check: %+v, query %q", r, query)
	}
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "ready")
	if r.code != exitError || !strings.Contains(r.stdout, `"ready": false`) || !strings.Contains(r.stdout, `"status": "fail"`) {
		t.Errorf("--json with a failing check: %+v", r)
	}

	// The update check's answer comes with the checks; only a newer release
	// is worth a line.
	out.Ready, out.Checks = true, out.Checks[:1]
	out.Update = &api.Update{Latest: "0.7.0", CheckedAt: t0}
	if r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "ready"); r.code != 0 || strings.Contains(r.stdout, "release") {
		t.Errorf("up to date: %+v", r)
	}
	out.Update = &api.Update{Latest: "0.8.0", Newer: true, CheckedAt: t0}
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "ready")
	if r.code != 0 || !strings.HasSuffix(r.stdout, "a newer release is available: 0.8.0 (checked 2026-10-01T14:00:00+02:00)\n") {
		t.Errorf("a newer release: %+v", r)
	}
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "ready")
	if r.code != 0 || !strings.Contains(r.stdout, `"latest": "0.8.0"`) || !strings.Contains(r.stdout, `"newer": true`) {
		t.Errorf("--json with a newer release: %+v", r)
	}
}
