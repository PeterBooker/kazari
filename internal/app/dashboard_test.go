package app_test

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

const testPassword = "correct horse battery"

var formTokenRE = regexp.MustCompile(`name="form_token" value="([^"]+)"`)

var setupLinkRE = regexp.MustCompile(`http://localhost/account/setup\?token=(kzl_[a-z2-7]+)`)

// origin is the Origin header a form of the page at rawURL is posted with.
func origin(rawURL string) http.Header {
	u, _ := url.Parse(rawURL)
	return http.Header{"Origin": {u.Scheme + "://" + u.Host}}
}

// signIn completes the sign-in that the redirect to signInURL started, with
// a password, and loads the callback it ends at. It returns the callback's
// answer.
func (b *browser) signIn(signInURL, login, password string) response {
	b.t.Helper()
	u, err := url.Parse(signInURL)
	if err != nil {
		b.t.Fatal(err)
	}
	q := u.Query()
	form := url.Values{"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}, "login": {login}, "password": {password}}
	callback := b.follow(b.send(http.MethodPost, "http://mcp.localhost/oauth/signin", form, origin("http://mcp.localhost")), "password sign-in")
	return b.get(callback)
}

// setPassword opens an invite, reset or setup link and sets a password
// through its page. It returns the answer to the form.
func (b *browser) setPassword(link string, extra url.Values) response {
	b.t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		b.t.Fatal(err)
	}
	clean := b.send(http.MethodGet, link, nil, nil)
	if clean.status != http.StatusSeeOther || clean.header.Get("Location") != u.Path {
		b.t.Fatalf("opening the link: status %d to %q, want 303 to %s", clean.status, clean.header.Get("Location"), u.Path)
	}
	page := u.Scheme + "://" + u.Host + u.Path
	shown := b.get(page)
	wantStatus(b.t, shown, http.StatusOK, "the link's page")
	token := formTokenRE.FindStringSubmatch(shown.body)
	if token == nil {
		b.t.Fatalf("the link's form carries no token: %s", truncate(shown.body))
	}
	form := url.Values{"password": {testPassword}, "confirm": {testPassword}, "form_token": {token[1]}}
	maps.Copy(form, extra)
	return b.send(http.MethodPost, page, form, origin(page))
}

// newAdmin invites an admin over the admin API and sets their password.
func newAdmin(t *testing.T, addr, handle string) {
	t.Helper()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	inv, err := admin.Invite(context.Background(), api.InviteRequest{Handle: handle, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if r := newBrowser(t, addr).setPassword(inv.Link.URL, nil); r.status != http.StatusSeeOther {
		t.Fatalf("setting the password: status %d: %s", r.status, truncate(r.body))
	}
}

// dashboard returns a browser signed in to the dashboard at base as handle.
func dashboard(t *testing.T, addr, base, handle string) *browser {
	t.Helper()
	b := newBrowser(t, addr)
	signIn := b.follow(b.get(base+"/"), "the dashboard without a session")
	if !strings.HasPrefix(signIn, "http://mcp.localhost/oauth/signin?") {
		t.Fatalf("sent to %q, want the sign-in host", signIn)
	}
	if r := b.signIn(signIn, handle, testPassword); r.status != http.StatusFound || r.header.Get("Location") != "/" {
		t.Fatalf("callback: status %d to %q: %s", r.status, r.header.Get("Location"), truncate(r.body))
	}
	return b
}

// A fresh instance with built-in accounts: the setup link from stderr makes
// the first admin, who signs in to the dashboard. Nothing along the way
// puts a token or a password into a log record or a span.
func TestSetupToDashboard(t *testing.T) {
	var stderr bytes.Buffer
	rec := &logRecorder{}
	spans := spanSink{tracetest.NewInMemoryExporter()}
	in := startWith(t, testConfig(t), app.Options{Stderr: &stderr, Logger: slog.New(rec), SpanExporter: spans})
	addr := in.app.Addrs().HTTP
	m := setupLinkRE.FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("stderr holds no setup link: %q", stderr.String())
	}
	link, token := m[0], m[1]

	// A preview fetch of the link, as a chat application makes, uses
	// nothing up.
	for range 2 {
		wantStatus(t, get(t, client(addr), link), http.StatusSeeOther, "a fetch of the link")
	}
	b := newBrowser(t, addr)
	done := b.setPassword(link, url.Values{"handle": {"peter"}, "name": {"Peter"}, "email": {"peter@example.com"}})
	if done.status != http.StatusSeeOther || done.header.Get("Location") != "/account/" {
		t.Fatalf("setup: status %d to %q: %s", done.status, done.header.Get("Location"), truncate(done.body))
	}
	// Setting the password signs the first admin in on the apex alone, and
	// the link is used up.
	welcome := b.get("http://localhost/account/")
	wantStatus(t, welcome, http.StatusOK, "the account page after setup")
	if !strings.Contains(welcome.body, "Welcome, <b>Peter</b>") || !strings.Contains(welcome.body, "http://mcp.localhost/") {
		t.Errorf("the welcome after setup = %q", truncate(welcome.body))
	}
	signIn := b.follow(b.get("http://admin.localhost/"), "the dashboard after setup")
	other := newBrowser(t, addr)
	other.send(http.MethodGet, link, nil, nil)
	wantStatus(t, other.get("http://localhost/account/setup"), http.StatusNotFound, "the setup link once a user exists")

	u, _ := url.Parse(signIn)
	if u.Host != "mcp.localhost" || u.Query().Get("host") != "@admin" {
		t.Fatalf("sign-in URL = %s", signIn)
	}
	cb := b.signIn(signIn, "peter@example.com", testPassword)
	if cb.status != http.StatusFound || cb.header.Get("Location") != "/" {
		t.Fatalf("callback: status %d to %q", cb.status, cb.header.Get("Location"))
	}
	overview := b.get("http://admin.localhost/")
	wantStatus(t, overview, http.StatusOK, "the overview")
	if !strings.Contains(overview.body, "Overview") || !strings.Contains(overview.body, "peter") {
		t.Errorf("overview = %q", truncate(overview.body))
	}

	// The session cookie opens the pages and nothing else: the JSON API
	// beside them reads bearer credentials alone.
	api := b.get("http://admin.localhost/v1/users")
	wantStatus(t, api, http.StatusUnauthorized, "the admin API with the dashboard's cookie")
	wantStatus(t, b.send(http.MethodPost, "http://admin.localhost/v1/jobs/gc/run", url.Values{}, origin("http://admin.localhost")),
		http.StatusUnauthorized, "a change on the admin API with the dashboard's cookie")

	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	events, err := admin.Audit(context.Background(), cli.AuditParams{Actions: []string{audit.DashboardSignIn}})
	if err != nil || len(events.Events) != 1 || events.Events[0].Actor != "peter" {
		t.Errorf("dashboard sign-in audit = %+v, %v", events, err)
	}

	in.stop()
	if err := <-in.done; err != nil {
		t.Fatal(err)
	}
	in.done <- nil
	secrets := []string{token, testPassword, "kzl_"}
	for _, r := range rec.all() {
		line := r.Message
		r.Attrs(func(a slog.Attr) bool { line += " " + a.Key + "=" + a.Value.String(); return true })
		for _, s := range secrets {
			if strings.Contains(line, s) {
				t.Errorf("a log record carries %q: %s", s, line)
			}
		}
	}
	// The whole path is traced: the link's page, the sign-in and the
	// dashboard's pages, each under its request's span.
	traced, session := map[string]bool{}, false
	for _, s := range spans.GetSpans() {
		traced[s.Name] = true
		for _, kv := range s.Attributes {
			if kv.Key == "kazari.credential.type" && kv.Value.AsString() == "session" {
				session = true
			}
		}
	}
	for _, name := range []string{"POST /account/setup", "user.redeem_link", "POST /oauth/signin", "user.check_password",
		"GET /session/callback", "oauth.redeem", "GET /{$}", "UserStore.UseLink"} {
		if !traced[name] {
			t.Errorf("no span %q", name)
		}
	}
	if !session {
		t.Error("no span of a dashboard request names its credential type, session")
	}
	for _, s := range spans.GetSpans() {
		for _, kv := range s.Attributes {
			for _, secret := range secrets {
				if strings.Contains(kv.Value.String(), secret) {
					t.Errorf("span %s attribute %s carries %q", s.Name, kv.Key, secret)
				}
			}
			if kv.Key == "url.path" && strings.Contains(kv.Value.String(), "peter") {
				t.Errorf("span %s carries a handle in its path: %s", s.Name, kv.Value.String())
			}
		}
	}
}

// With admin.listen the admin host leaves the public listener, and the
// dashboard is reached at admin.public_url: here plain http on this machine, as
// through an SSH tunnel.
func TestDashboardAtAdminURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := testConfig(t)
	cfg.Admin.Listen = "127.0.0.1:" + strconv.Itoa(port)
	cfg.Admin.PublicURL = "http://localhost:" + strconv.Itoa(port)
	in := start(t, cfg)
	addr := in.app.Addrs().HTTP

	wantStatus(t, get(t, client(addr), "http://admin.localhost/"), http.StatusNotFound, "the admin host on the public listener")
	local := cli.NewClient("http://kazari", "", socketClient(in.app.Addrs().Socket))
	inv, err := local.Invite(context.Background(), api.InviteRequest{Handle: "peter", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if r := newBrowser(t, addr).setPassword(inv.Link.URL, nil); r.status != http.StatusSeeOther {
		t.Fatalf("setting the password: status %d", r.status)
	}

	// One browser, two listeners: the public one for the sign-in host, the
	// admin one for the dashboard.
	b := newBrowser(t, addr)
	tunnel := client(in.app.Addrs().Admin)
	tunnel.Jar = b.hc.Jar
	dash := &browser{t: t, hc: tunnel}

	// Whatever Host a request names, the sign-in comes back to admin.public_url.
	req, _ := http.NewRequest(http.MethodGet, cfg.Admin.PublicURL+"/people", nil)
	req.Host = "admin.evil.example"
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	resp, err := tunnel.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	signIn := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(signIn, "http://mcp.localhost/oauth/signin?") {
		t.Fatalf("the dashboard without a session: status %d to %q", resp.StatusCode, signIn)
	}
	u, _ := url.Parse(signIn)
	q := u.Query()
	form := url.Values{"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}, "login": {"peter"}, "password": {testPassword}}
	callback := b.follow(b.send(http.MethodPost, "http://mcp.localhost/oauth/signin", form, origin("http://mcp.localhost")), "password sign-in")
	if !strings.HasPrefix(callback, cfg.Admin.PublicURL+"/session/callback?code=") || strings.Contains(callback, "evil") {
		t.Fatalf("callback = %q, want it at admin.public_url", callback)
	}

	// The forged Host set the nonce cookie for another name, so start over
	// as a browser that types admin.public_url. It is signed in at the sign-in
	// host by now, which sends it straight back.
	signIn = dash.follow(dash.get(cfg.Admin.PublicURL+"/people"), "the dashboard without a session")
	callback = b.follow(b.get(signIn), "a signed-in browser at the sign-in host")
	cb := dash.get(callback)
	if cb.status != http.StatusFound || cb.header.Get("Location") != "/people" {
		t.Fatalf("callback: status %d to %q: %s", cb.status, cb.header.Get("Location"), truncate(cb.body))
	}
	// Plain http cannot carry a __Host- cookie.
	jarURL, _ := url.Parse(cfg.Admin.PublicURL)
	var names []string
	for _, c := range tunnel.Jar.Cookies(jarURL) {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "kazari_admin") || strings.Contains(strings.Join(names, " "), "__Host-") {
		t.Errorf("cookies at admin.public_url = %v, want the unprefixed session cookie", names)
	}
	wantStatus(t, dash.get(cfg.Admin.PublicURL+"/people"), http.StatusOK, "People through the tunnel")
}

// admin.listen without admin.public_url: nothing names where a browser reaches
// the admin host, so the dashboard is off and the API is as it was.
func TestDashboardOffWithoutURL(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Listen = "127.0.0.1:0"
	in := start(t, cfg)
	private := client(in.app.Addrs().Admin)
	root := get(t, private, "http://admin.localhost/")
	if root.status != http.StatusNotFound || !strings.Contains(root.body, `"not_found"`) {
		t.Errorf("the admin host's root: status %d: %s", root.status, truncate(root.body))
	}
	wantStatus(t, do(t, private, http.MethodGet, "http://admin.localhost/v1/users", adminToken, ""), http.StatusOK, "the admin API")
	q := url.Values{"host": {"@admin"}, "path": {"/"}, "nonce": {strings.Repeat("a", 43)}}
	wantStatus(t, get(t, client(in.app.Addrs().HTTP), "http://mcp.localhost/oauth/signin?"+q.Encode()), http.StatusBadRequest,
		"a sign-in for a dashboard that is off")
}

func TestDashboardSwitchedOff(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Dashboard = false
	in := start(t, cfg)
	root := get(t, client(in.app.Addrs().HTTP), "http://admin.localhost/")
	if root.status != http.StatusNotFound || !strings.Contains(root.body, `"not_found"`) {
		t.Errorf("the admin host's root with admin.dashboard off: status %d: %s", root.status, truncate(root.body))
	}
}

// serve --local offers passkeys, so they can be tried without a domain.
func TestLocalPresetOffersPasskeys(t *testing.T) {
	cfg := config.Local()
	cfg.DataDir = t.TempDir()
	cfg.Listen.HTTP = "127.0.0.1:0"
	in := start(t, cfg)
	b := newBrowser(t, in.app.Addrs().HTTP)
	signIn := b.follow(b.get("http://admin.kazari.localhost:8080/"), "the dashboard without a session")
	if !strings.HasPrefix(signIn, "http://mcp.kazari.localhost:8080/oauth/signin?") {
		t.Fatalf("sent to %q, want the sign-in host", signIn)
	}
	page := b.get(signIn)
	wantStatus(t, page, http.StatusOK, "the sign-in page")
	if !strings.Contains(page.body, `data-passkey="get"`) {
		t.Errorf("the sign-in page of serve --local has no passkey form: %s", truncate(page.body))
	}
}

// The account pages live on the apex: an invited member sets a password,
// signs in and issues a key, which holds member scopes at most.
func TestAccountOnApex(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	inv, err := admin.Invite(ctx, api.InviteRequest{Handle: "ana", Email: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inv.Link.URL, "http://localhost/account/invite?token=kzl_") {
		t.Fatalf("invite link = %q", inv.Link.URL)
	}
	b := newBrowser(t, addr)
	done := b.setPassword(inv.Link.URL, nil)
	if done.status != http.StatusSeeOther || done.header.Get("Location") != "/account/" {
		t.Fatalf("invite: status %d to %q", done.status, done.header.Get("Location"))
	}
	// The invite signed her in: the account page opens without a sign-in.
	page := b.get("http://localhost/account/")
	wantStatus(t, page, http.StatusOK, "the account page")
	token := formTokenRE.FindStringSubmatch(page.body)
	if token == nil {
		t.Fatalf("no form token on the account page: %s", truncate(page.body))
	}
	once := regexp.MustCompile(`name="once" value="([^"]+)"`).FindStringSubmatch(page.body)
	if once == nil {
		t.Fatalf("no single-use value on the account page: %s", truncate(page.body))
	}
	issued := b.send(http.MethodPost, "http://localhost/account/keys",
		url.Values{"form_token": {token[1]}, "once": {once[1]}, "label": {"laptop"}}, origin("http://localhost"))
	wantStatus(t, issued, http.StatusOK, "issuing a key")
	key := regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`).FindString(issued.body)
	if key == "" {
		t.Fatalf("the new key is not on the page: %s", truncate(issued.body))
	}
	me, err := cli.NewClient("http://api.localhost", key, client(addr)).Me(ctx)
	if err != nil || me.Handle != "ana" {
		t.Fatalf("the key from the account page: %+v, %v", me, err)
	}
	// The dashboard is not theirs: she signs in at the sign-in host, and
	// the dashboard refuses the member.
	toSignIn := b.follow(b.get("http://admin.localhost/"), "the dashboard as a member")
	if cb := b.signIn(toSignIn, "ana", testPassword); cb.status != http.StatusFound {
		t.Fatalf("sign-in for the dashboard: status %d: %s", cb.status, truncate(cb.body))
	}
	wantStatus(t, b.get("http://admin.localhost/"), http.StatusForbidden, "the dashboard as a member")
}

// A backup taken over the admin socket unpacks into a data directory a new
// instance serves every page from.
func TestBackupRestores(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	user := cli.NewClient("http://api.localhost", issued.Key, client(addr))
	pages := map[string]string{"one": "<h1>one</h1>", "two": "<h1>two</h1>"}
	for slug, body := range pages {
		if _, err := user.Create(ctx, api.CreateArtifactRequest{Slug: slug, Files: []api.FileInput{{Path: "index.html", Content: body}}}); err != nil {
			t.Fatal(err)
		}
	}

	local := cli.NewClient("http://kazari", "", socketClient(in.app.Addrs().Socket))
	body, err := local.Backup(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "data")
	err = backup.Extract(body, restored)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"uploads", "tmp", "admin.sock"} {
		if _, err := os.Stat(filepath.Join(restored, name)); err == nil {
			t.Errorf("the archive holds %s", name)
		}
	}
	// Off the socket the certificates never leave the host.
	if _, err := admin.Backup(ctx, true); !cli.IsCode(err, api.CodeForbidden) {
		t.Errorf("a backup with certificates over the network: err = %v, want forbidden", err)
	}
	events, err := admin.Audit(ctx, cli.AuditParams{Actions: []string{audit.BackupCreate}})
	if err != nil || len(events.Events) != 1 {
		t.Errorf("backup audit = %+v, %v", events, err)
	}

	in.stop()
	if err := <-in.done; err != nil {
		t.Fatal(err)
	}
	in.done <- nil
	cfg2 := testConfig(t)
	cfg2.DataDir = restored
	in2 := start(t, cfg2)
	for slug, want := range pages {
		got := get(t, client(in2.app.Addrs().HTTP), "http://"+slug+".localhost/")
		if got.status != http.StatusOK || got.body != want {
			t.Errorf("%s from the restored instance: status %d: %q", slug, got.status, truncate(got.body))
		}
	}
	if me, err := cli.NewClient("http://api.localhost", issued.Key, client(in2.app.Addrs().HTTP)).Me(ctx); err != nil || me.Handle != "peter" {
		t.Errorf("the key on the restored instance: %+v, %v", me, err)
	}
}

// The update check is a job like the others, run here through the admin
// API against a stand-in for the releases API.
func TestUpdateCheckJob(t *testing.T) {
	var latest string
	var fail bool
	var asked int
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"` + latest + `"}`))
	}))
	defer releases.Close()

	in := startWith(t, testConfig(t), app.Options{UpdateURL: releases.URL, Version: "0.7.0"})
	addr := in.app.Addrs().HTTP
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	ctx := context.Background()
	newAdmin(t, addr, "peter")
	b := dashboard(t, addr, "http://admin.localhost", "peter")
	system := func() string {
		t.Helper()
		r := b.get("http://admin.localhost/system")
		wantStatus(t, r, http.StatusOK, "System")
		return r.body
	}

	latest = "0.7.0"
	if err := admin.RunJob(ctx, "update-check"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(system(), "0.8.0") {
		t.Error("System names a newer version when the newest is the one running")
	}
	latest = "0.8.0"
	if err := admin.RunJob(ctx, "update-check"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system(), "0.8.0") {
		t.Error("System does not name the newer version")
	}
	overview := b.get("http://admin.localhost/")
	if !strings.Contains(overview.body, "0.8.0") {
		t.Error("the overview does not list the newer version")
	}
	// A failed check is the job's error, and keeps what was known.
	fail = true
	if err := admin.RunJob(ctx, "update-check"); err == nil {
		t.Error("a failed check reported success")
	}
	if !strings.Contains(system(), "0.8.0") {
		t.Error("a failed check forgot the newer version")
	}
	if asked != 3 {
		t.Errorf("the releases API was asked %d times, want 3", asked)
	}
}

func TestUpdateCheckOff(t *testing.T) {
	releases := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the releases API was asked with admin.update_check off")
	}))
	defer releases.Close()
	cfg := testConfig(t)
	cfg.Admin.UpdateCheck = false
	in := startWith(t, cfg, app.Options{UpdateURL: releases.URL, Version: "0.7.0"})
	admin := cli.NewClient("http://admin.localhost", adminToken, client(in.app.Addrs().HTTP))
	if err := admin.RunJob(context.Background(), "update-check"); !cli.IsCode(err, api.CodeNotFound) {
		t.Errorf("running the update check with it off: err = %v, want not_found", err)
	}
	// A build without a version has nothing to compare.
	in = startWith(t, testConfig(t), app.Options{UpdateURL: releases.URL, Version: "dev"})
	admin = cli.NewClient("http://admin.localhost", adminToken, client(in.app.Addrs().HTTP))
	if err := admin.RunJob(context.Background(), "update-check"); !cli.IsCode(err, api.CodeNotFound) {
		t.Errorf("running the update check on an unstamped build: err = %v, want not_found", err)
	}
}

func TestReadyAndHealth(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	wantStatus(t, get(t, client(addr), "http://admin.localhost/v1/ready"), http.StatusUnauthorized, "readiness without a credential")
	ready, err := cli.NewClient("http://admin.localhost", adminToken, client(addr)).Ready(context.Background(), true)
	if err != nil || len(ready.Checks) == 0 {
		t.Fatalf("Ready = %+v, %v", ready, err)
	}
	for _, host := range []string{"http://admin.localhost/health", "http://admin.localhost/v1/health", "http://localhost/health"} {
		h := get(t, client(addr), host)
		if h.status != http.StatusOK || !strings.Contains(h.body, `"status":"ok"`) || strings.Contains(h.body, "check") {
			t.Errorf("%s: status %d: %s", host, h.status, h.body)
		}
	}
}

// What the new surfaces record reaches the scrape under the names the
// documentation gives.
func TestAccountAndDashboardMetrics(t *testing.T) {
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"tag_name":"0.8.0"}`))
	}))
	defer releases.Close()
	in := startWith(t, testConfig(t), app.Options{UpdateURL: releases.URL, Version: "0.7.0"})
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))

	newAdmin(t, addr, "peter")
	b := dashboard(t, addr, "http://admin.localhost", "peter")
	wantStatus(t, b.get("http://admin.localhost/people"), http.StatusOK, "People")
	if _, err := admin.Ready(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := admin.RunJob(ctx, "update-check"); err != nil {
		t.Fatal(err)
	}
	body, err := cli.NewClient("http://kazari", "", socketClient(in.app.Addrs().Socket)).Backup(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Extract(body, filepath.Join(t.TempDir(), "data")); err != nil {
		t.Fatal(err)
	}
	body.Close()

	scrape := get(t, client(addr), "http://admin.localhost/metrics")
	wantStatus(t, scrape, http.StatusOK, "the scrape")
	for _, want := range []string{
		`kazari_auth_requests_total{kazari_auth_result="ok",kazari_credential_type="session",kazari_surface="admin"`,
		`kazari_auth_requests_total{kazari_auth_result="missing",kazari_credential_type="session",kazari_surface="admin"`,
		`kazari_user_sign_ins_total{kazari_sign_in_method="password",kazari_sign_in_result="ok"`,
		`kazari_user_link_operations_total{kazari_user_link_operation="use",kazari_user_link_purpose="invite"`,
		`kazari_user_password_hash_duration_seconds_count{kazari_user_password_operation="verify"`,
		"\nkazari_backup_duration_seconds_count 1\n",
		"\nkazari_backup_size_bytes_count 1\n",
		"\nkazari_backup_last_success_timestamp_seconds ",
		`kazari_ready_checks{kazari_ready_check="config",kazari_ready_status="ok"`,
		"\nkazari_ready_timestamp_seconds ",
		"\nkazari_update_available 1\n",
		`kazari_surface="account"`,
	} {
		if !strings.Contains(scrape.body, want) {
			t.Errorf("the scrape lacks %s", want)
		}
	}
	// No person and no credential is a label.
	for _, secret := range []string{"peter", testPassword, "kzl_"} {
		if strings.Contains(scrape.body, secret) {
			t.Errorf("the scrape carries %q", secret)
		}
	}
}

// A browser that opens a host whose root is no page finds words and links,
// not an error: the apex, the connector address and the API address. A
// client of the API or of MCP gets what it always got.
func TestFrontPages(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	b := newBrowser(t, addr)
	for _, tt := range []struct {
		name, url string
		want      []string
	}{
		{"the apex", "http://localhost/", []string{"Sign in to your account", `href="http://localhost/account/"`, `value="http://mcp.localhost/"`, "Open the dashboard"}},
		{"the connector address", "http://mcp.localhost/", []string{"The connector <b>address</b>", `value="http://mcp.localhost/"`, `href="/oauth/assets/theme.`}},
		{"the API address", "http://api.localhost/", []string{"The API <b>address</b>", "export KAZARI_URL=http://api.localhost", `href="/assets/theme.`}},
	} {
		r := b.get(tt.url)
		wantStatus(t, r, http.StatusOK, tt.name)
		if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") || r.header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: Content-Type %q, CSP %q", tt.name, r.header.Get("Content-Type"), r.header.Get("Content-Security-Policy"))
		}
		for _, want := range tt.want {
			if !strings.Contains(r.body, want) {
				t.Errorf("%s lacks %q", tt.name, want)
			}
		}
		for _, bad := range []string{"An Kazari", "<style", " style="} {
			if strings.Contains(r.body, bad) {
				t.Errorf("%s holds %q", tt.name, bad)
			}
		}
		css := regexp.MustCompile(`href="(/[a-z/]+/theme\.[^"]+\.css)"`).FindStringSubmatch(r.body)
		if css == nil {
			t.Errorf("%s loads no stylesheet", tt.name)
			continue
		}
		host := strings.TrimSuffix(tt.url, "/")
		wantStatus(t, get(t, client(addr), host+css[1]), http.StatusOK, tt.name+": its stylesheet")
	}
	api := get(t, client(addr), "http://api.localhost/")
	if strings.HasPrefix(api.header.Get("Content-Type"), "text/html") {
		t.Errorf("a client of the API at its root got the page: %q", truncate(api.body))
	}
}
