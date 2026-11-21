package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestAdminUsersAndKeys(t *testing.T) {
	e := newEnv(t, config{})

	var u api.User
	e.call(http.MethodPost, e.a("/v1/users"), adminToken, api.CreateUserRequest{
		Handle: "carol", Email: "Carol@Example.com", QuotaBytes: ptr[int64](1 << 20),
	}, http.StatusCreated, &u)
	if u.Handle != "carol" || u.Email != "carol@example.com" || u.Role != "member" || u.QuotaBytes != 1<<20 {
		t.Errorf("created user = %+v", u)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"carol"}`)}), 409, api.CodeHandleTaken)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"X"}`)}), 422, api.CodeInvalidArgument)

	var users []api.User
	e.call(http.MethodGet, e.a("/v1/users"), adminToken, nil, http.StatusOK, &users)
	if len(users) != 2 || users[0].Handle != "alice" || users[1].Handle != "carol" {
		t.Errorf("users = %+v", users)
	}

	var issued api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/carol/keys"), adminToken, api.IssueKeyRequest{
		Label: "laptop", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, ExpiresIn: "720h",
	}, http.StatusCreated, &issued)
	if issued.Key == "" || issued.APIKey.Label != "laptop" || issued.APIKey.ExpiresAt == nil ||
		!issued.APIKey.ExpiresAt.Equal(t0.Add(720*time.Hour)) {
		t.Errorf("issued = %+v", issued)
	}
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), issued.Key, nil, http.StatusOK, &me)
	if me.Handle != "carol" || me.QuotaBytes != 1<<20 {
		t.Errorf("me = %+v", me)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/keys"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"expires_in":"soon"}`)}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/keys"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"scopes":["admin"]}`)}), 422, api.CodeInvalidArgument)

	var second api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/carol/keys"), adminToken, nil, http.StatusCreated, &second)
	if !slices.Equal(second.APIKey.Scopes, auth.MemberScopes) {
		t.Errorf("default scopes = %v", second.APIKey.Scopes)
	}
	var keys []api.APIKey
	e.call(http.MethodGet, e.a("/v1/users/carol/keys"), adminToken, nil, http.StatusOK, &keys)
	if len(keys) != 2 {
		t.Errorf("keys = %+v", keys)
	}
	e.call(http.MethodDelete, e.a("/v1/users/carol/keys/"+second.APIKey.Prefix), adminToken, nil, http.StatusNoContent, nil)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/me"), token: second.Key}), 401, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/users/carol/keys/nope1234"), token: adminToken}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users/nobody/keys"), token: adminToken}), 404, api.CodeNotFound)

	e.call(http.MethodPost, e.a("/v1/users/carol/identities"), adminToken,
		api.LinkIdentityRequest{Issuer: "https://idp.test", Subject: "c-1", Email: "carol@example.com"}, http.StatusNoContent, nil)
	e.call(http.MethodGet, e.a("/v1/users/carol"), adminToken, nil, http.StatusOK, &u)
	if len(u.Identities) != 1 || u.Identities[0].Issuer != "https://idp.test" || u.Identities[0].Subject != "c-1" {
		t.Errorf("identities = %+v", u.Identities)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/identities"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"issuer":"x"}`)}), 422, api.CodeInvalidArgument)
	e.call(http.MethodPost, e.a("/v1/users/carol/identities/unlink"), adminToken,
		api.LinkIdentityRequest{Issuer: "https://idp.test", Subject: "c-1"}, http.StatusNoContent, nil)
	e.call(http.MethodGet, e.a("/v1/users/carol"), adminToken, nil, http.StatusOK, &u)
	if len(u.Identities) != 0 {
		t.Errorf("identities after unlink = %+v", u.Identities)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/identities/unlink"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"issuer":"https://idp.test","subject":"c-1"}`)}), 404, api.CodeNotFound)

	e.call(http.MethodPatch, e.a("/v1/users/carol"), adminToken, api.UpdateUserRequest{Disabled: ptr(true), DisplayName: ptr("Carol")}, http.StatusOK, &u)
	if u.DisabledAt == nil || u.DisplayName != "Carol" {
		t.Errorf("disabled user = %+v", u)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/me"), token: issued.Key}), 401, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodPatch, url: e.a("/v1/users/carol"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"role":"root"}`)}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users/nobody"), token: adminToken}), 404, api.CodeNotFound)
}

func TestAdminArtifacts(t *testing.T) {
	e := newEnv(t, config{})
	e.member("bob", nil)
	e.create(e.alice, "hidden", func(r *api.CreateArtifactRequest) { r.Visibility = "unlisted" })
	e.create(e.alice, "shown")

	var l api.ArtifactList
	e.call(http.MethodGet, e.a("/v1/artifacts"), adminToken, nil, http.StatusOK, &l)
	if len(l.Items) != 2 {
		t.Errorf("admin list = %+v", l.Items)
	}

	var a api.Artifact
	e.call(http.MethodPost, e.a("/v1/artifacts/hidden/transfer"), adminToken, api.TransferRequest{Owner: "bob"}, http.StatusOK, &a)
	if a.Owner != "bob" || a.Slug != "hidden" {
		t.Errorf("transferred = %+v", a)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/artifacts/hidden/transfer"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"owner":"nobody"}`)}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/artifacts/missing/transfer"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"owner":"bob"}`)}), 404, api.CodeNotFound)

	e.call(http.MethodGet, e.a("/v1/artifacts?owner=bob"), adminToken, nil, http.StatusOK, &l)
	if len(l.Items) != 1 || l.Items[0].Slug != "hidden" {
		t.Errorf("owner=bob = %+v", l.Items)
	}

	e.call(http.MethodPost, e.a("/v1/artifacts/shown/hide"), adminToken, nil, http.StatusOK, &a)
	if a.UnpublishedAt == nil {
		t.Errorf("hidden artifact = %+v", a)
	}

	for query, want := range map[string][]string{
		"?q=HID":                   {"hidden"},
		"?q=nothing":               {},
		"?visibility=unlisted":     {"hidden"},
		"?visibility=unpublished":  {"shown"},
		"?visibility=public":       {},
		"?visibility=":             {"hidden", "shown"},
		"?q=h&visibility=unlisted": {"hidden"},
	} {
		e.call(http.MethodGet, e.a("/v1/artifacts"+query), adminToken, nil, http.StatusOK, &l)
		got := []string{}
		for _, it := range l.Items {
			got = append(got, it.Slug)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("admin list %s = %v, want %v", query, got, want)
		}
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/artifacts?visibility=private"), token: adminToken}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/artifacts?q=" + strings.Repeat("a", 101)), token: adminToken}), 422, api.CodeInvalidArgument)
}

func TestAdminTransferAll(t *testing.T) {
	e := newEnv(t, config{})
	bob := e.member("bob", nil)
	e.create(e.alice, "one")
	e.create(e.alice, "two")
	e.create(bob, "kept")
	transfer := func(token, from, body string) *http.Response {
		return e.send(req{method: http.MethodPost, url: e.a("/v1/users/" + from + "/transfer"), token: token,
			contentType: "application/json", body: strings.NewReader(body)})
	}

	e.apiErr(transfer(e.alice, "alice", `{"owner":"bob"}`), 403, api.CodeForbidden)
	e.apiErr(transfer(adminToken, "alice", `{"owner":"nobody"}`), 422, api.CodeInvalidArgument)
	e.apiErr(transfer(adminToken, "nobody", `{"owner":"bob"}`), 422, api.CodeInvalidArgument)
	e.apiErr(transfer(adminToken, "alice", `{"owner":"alice"}`), 422, api.CodeInvalidArgument)
	e.apiErr(transfer(adminToken, "alice", `{}`), 422, api.CodeInvalidArgument)

	var out api.TransferredArtifacts
	e.expect(transfer(adminToken, "alice", `{"owner":"bob"}`), http.StatusOK, &out)
	if !slices.Equal(out.Transferred, []string{"one", "two"}) {
		t.Errorf("transferred = %v, want one and two", out.Transferred)
	}
	var l api.ArtifactList
	e.call(http.MethodGet, e.a("/v1/artifacts?owner=bob"), adminToken, nil, http.StatusOK, &l)
	if len(l.Items) != 3 {
		t.Errorf("bob owns %d artifacts, want 3", len(l.Items))
	}
	var events api.AuditList
	e.call(http.MethodGet, e.a("/v1/audit?action=artifact.transfer"), adminToken, nil, http.StatusOK, &events)
	if len(events.Events) != 2 || events.Events[0].Details["from"] != "alice" || events.Events[0].Details["to"] != "bob" {
		t.Errorf("transfer events = %+v", events.Events)
	}

	resp := transfer(adminToken, "alice", `{"owner":"bob"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"transferred":[]}` {
		t.Errorf("second transfer = %d %s, want an empty list", resp.StatusCode, raw)
	}
}

func TestAdminJobsAuditSettingsHealth(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo")

	var jobs []api.JobStatus
	e.call(http.MethodGet, e.a("/v1/jobs"), adminToken, nil, http.StatusOK, &jobs)
	if len(jobs) != 1 || jobs[0].Name != "count" || jobs[0].Every != "1h0m0s" || jobs[0].LastRun != nil {
		t.Errorf("jobs = %+v", jobs)
	}
	var js api.JobStatus
	e.call(http.MethodPost, e.a("/v1/jobs/count/run"), adminToken, nil, http.StatusOK, &js)
	if e.jobRuns.Load() != 1 || js.LastRun == nil || !js.LastRun.Equal(t0) {
		t.Errorf("after run: runs %d, status %+v", e.jobRuns.Load(), js)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/jobs/nope/run"), token: adminToken}), 404, api.CodeNotFound)

	auditList := func(query string) api.AuditList {
		t.Helper()
		var l api.AuditList
		e.call(http.MethodGet, e.a("/v1/audit"+query), adminToken, nil, http.StatusOK, &l)
		return l
	}
	if events := auditList("?slug=demo").Events; len(events) != 1 || events[0].Action != audit.ArtifactCreate || events[0].Actor != "alice" {
		t.Errorf("audit = %+v", events)
	}
	hourAgo, hourAhead := t0.Add(-time.Hour).Format(time.RFC3339), t0.Add(time.Hour).Format(time.RFC3339)
	byActor := auditList("?actor=alice&limit=10&since=" + hourAgo)
	if len(byActor.Events) != 2 || byActor.NextCursor != "" { // the key issued in setup and the create
		t.Errorf("audit by actor = %+v", byActor)
	}
	if events := auditList("?actor=alice&until=" + hourAgo).Events; events == nil || len(events) != 0 {
		t.Errorf("audit until an hour ago = %#v, want an empty list", events)
	}
	if events := auditList("?actor=alice&until=" + hourAhead).Events; len(events) != 2 {
		t.Errorf("audit until an hour ahead = %+v", events)
	}
	if events := auditList("?action=artifact.").Events; len(events) != 1 || events[0].Action != audit.ArtifactCreate {
		t.Errorf("audit by action prefix = %+v", events)
	}
	if events := auditList("?action=key.issue&action=artifact.create&actor=alice").Events; len(events) != 2 {
		t.Errorf("audit by two actions = %+v", events)
	}
	if events := auditList("?action=artifact.publish").Events; len(events) != 0 {
		t.Errorf("audit by an action nobody took = %+v", events)
	}
	first := auditList("?actor=alice&limit=1")
	if len(first.Events) != 1 || first.NextCursor == "" {
		t.Fatalf("first audit page = %+v, want one event and a cursor", first)
	}
	second := auditList("?actor=alice&limit=1&cursor=" + first.NextCursor)
	if len(second.Events) != 1 || second.NextCursor != "" || second.Events[0].Action == first.Events[0].Action {
		t.Errorf("second audit page = %+v after %+v", second, first)
	}
	for _, q := range []string{"?since=yesterday", "?until=tomorrow", "?cursor=nope", "?action="} {
		e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/audit" + q), token: adminToken}), 422, api.CodeInvalidArgument)
	}

	var settings map[string]any
	e.call(http.MethodGet, e.a("/v1/settings"), adminToken, nil, http.StatusOK, &settings)
	if settings["base_domain"] != "art.test" {
		t.Errorf("settings = %v", settings)
	}

	for _, p := range []string{"/health", "/v1/health"} {
		var h api.Health
		e.call(http.MethodGet, e.a(p), "", nil, http.StatusOK, &h)
		if h.Status != "ok" || h.Version == "" {
			t.Errorf("%s = %+v", p, h)
		}
	}
	resp := e.send(req{method: http.MethodGet, url: e.a("/metrics")})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics status %d", resp.StatusCode)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users"), token: e.alice}), 403, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users"), token: "wrong"}), 401, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users")}), 401, api.CodeUnauthorized)

	if _, err := e.users.CreateUser(e.ctx, userReq("root", "admin")); err != nil {
		t.Fatal(err)
	}
	_, rootKey, err := e.users.IssueKey(e.ctx, user.IssueKeyRequest{Handle: "root", Scopes: []string{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	e.call(http.MethodGet, e.a("/v1/users"), rootKey, nil, http.StatusOK, nil)

	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/nothing"), token: adminToken}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users/alice/grants"), token: adminToken}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/nothing")}), 404, api.CodeNotFound)
	resp = e.send(req{method: http.MethodPost, url: e.a("/health")})
	e.apiErr(resp, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	resp = e.send(req{method: http.MethodDelete, url: e.a("/v1/users"), token: adminToken})
	e.apiErr(resp, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestAdminJobRunning(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	jobs := scheduler.New(scheduler.Options{}, scheduler.Job{Name: "slow", Run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}})
	srv := httpapiAdmin(t, httpapi.AdminDeps{Jobs: jobs})
	var wg sync.WaitGroup
	wg.Go(func() { _ = jobs.Trigger(context.Background(), "slow") })
	<-started
	e := &env{t: t, ctx: context.Background(), admURL: srv}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/jobs/slow/run"), token: adminToken}), 409, "job_running")
	close(release)
	wg.Wait()
}

type fakeGrants struct {
	mu     sync.Mutex
	grants []api.OAuthGrant
}

func (f *fakeGrants) ListGrants(_ context.Context, handle string) ([]api.OAuthGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.OAuthGrant
	for _, g := range f.grants {
		if g.User == handle && g.RevokedAt == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeGrants) RevokeGrant(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.grants {
		if f.grants[i].ID == id {
			f.grants[i].RevokedAt = &t0
			return nil
		}
	}
	return httpapi.ErrGrantNotFound
}

func TestAdminGrants(t *testing.T) {
	grants := &fakeGrants{grants: []api.OAuthGrant{{ID: 7, ClientID: "c", User: "alice", Scopes: []string{auth.ScopeRead}}}}
	e := newEnv(t, config{grants: grants})
	var got []api.OAuthGrant
	e.call(http.MethodGet, e.a("/v1/users/alice/grants"), adminToken, nil, http.StatusOK, &got)
	if len(got) != 1 || got[0].ID != 7 {
		t.Errorf("grants = %+v", got)
	}
	e.call(http.MethodDelete, e.a("/v1/grants/7"), adminToken, nil, http.StatusNoContent, nil)
	e.call(http.MethodGet, e.a("/v1/users/alice/grants"), adminToken, nil, http.StatusOK, &got)
	if len(got) != 0 {
		t.Errorf("grants after revoke = %+v", got)
	}
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/grants/99"), token: adminToken}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/grants/x"), token: adminToken}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users/nobody/grants"), token: adminToken}), 404, api.CodeNotFound)
}

func httpapiAdmin(t *testing.T, d httpapi.AdminDeps) string {
	t.Helper()
	return httptestServer(t, httpapi.NewAdminAPI(d, httpapi.AdminOptions{AdminToken: adminToken}))
}

func TestAdminIPRateLimit(t *testing.T) {
	e := newEnv(t, config{admin: httpapi.AdminOptions{IPRatePerMinute: 2}})
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users"), token: "guess-1"}), http.StatusUnauthorized, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users"), token: "guess-2"}), http.StatusUnauthorized, api.CodeUnauthorized)
	resp := e.send(req{method: http.MethodGet, url: e.a("/v1/users"), token: adminToken})
	e.apiErr(resp, http.StatusTooManyRequests, api.CodeRateLimited)
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After = %q, want 30", ra)
	}
	for _, p := range []string{"/health", "/v1/health"} {
		e.call(http.MethodGet, e.a(p), "", nil, http.StatusOK, nil)
	}
}

func TestAdminMetricsToken(t *testing.T) {
	const token = "scrape-0123456789abcdef0123456789"
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("up 1\n")) })
	get := func(base, bearer string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	guarded := httptestServer(t, httpapi.NewAdminAPI(httpapi.AdminDeps{Metrics: metrics}, httpapi.AdminOptions{MetricsToken: token}))
	for _, bearer := range []string{"", "wrong", token + "x", adminToken} {
		resp := get(guarded, bearer)
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q: status %d, WWW-Authenticate %q; want 401 with a Bearer challenge",
				bearer, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if resp := get(guarded, token); resp.StatusCode != http.StatusOK {
		t.Errorf("right token: status %d", resp.StatusCode)
	}

	open := httptestServer(t, httpapi.NewAdminAPI(httpapi.AdminDeps{Metrics: metrics}, httpapi.AdminOptions{}))
	if resp := get(open, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("without MetricsToken: status %d, want 200", resp.StatusCode)
	}
	absent := httptestServer(t, httpapi.NewAdminAPI(httpapi.AdminDeps{}, httpapi.AdminOptions{MetricsToken: token}))
	if resp := get(absent, token); resp.StatusCode != http.StatusNotFound {
		t.Errorf("without Metrics: status %d, want 404", resp.StatusCode)
	}
}

func TestAPIErrorsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	tel := instrument.Providers{MeterProvider: mp}
	h := middleware.NewObserver(tel, false).Observe(instrument.SurfaceAdmin, middleware.ObserveOptions{})(
		httpapi.NewAdminAPI(httpapi.AdminDeps{}, httpapi.AdminOptions{AdminToken: adminToken, Telemetry: tel}))
	base := httptestServer(t, h)
	r, _ := http.NewRequest(http.MethodGet, base+"/v1/nothing", nil)
	r.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kazari.api.errors" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				surface, _ := dp.Attributes.Value(instrument.Surface)
				code, _ := dp.Attributes.Value(instrument.ErrorCode)
				found = dp.Value == 1 && surface.AsString() == "admin" && code.AsString() == api.CodeNotFound
			}
		}
	}
	if !found {
		t.Errorf("kazari.api.errors lacks admin/not_found: %+v", rm.ScopeMetrics)
	}
}
