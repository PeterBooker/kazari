package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

func mcpSession(t *testing.T, addr, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "routine", Version: "v0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://mcp.localhost/",
		HTTPClient:           &http.Client{Transport: bearer{token, pinnedTransport(addr)}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func tokenRequest(t *testing.T, hc *http.Client, form url.Values) tokens {
	t.Helper()
	resp, err := hc.PostForm("http://mcp.localhost/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out tokens
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// An agent that runs with nobody present: a key restricted to its job, the
// same key as an OAuth connection, and the lifetimes of auth.tokens.
func TestUnattendedAgentEndToEnd(t *testing.T) {
	const day = 24 * time.Hour
	cfg := testConfig(t)
	cfg.Auth.Tokens = config.Tokens{
		AccessTTL:          config.Duration(30 * time.Minute),
		RefreshTTL:         config.Duration(45 * day),
		RefreshReuseWindow: config.Duration(30 * time.Second),
		MaxKeyTTL:          config.Duration(365 * day),
	}
	in := start(t, cfg)
	addr := in.app.Addrs().HTTP
	hc := client(addr)
	ctx := context.Background()
	now := in.clock.Now()

	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "ana"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "ana", api.IssueKeyRequest{Label: "weekly-report",
		Scopes: []string{"artifacts:read", "artifacts:write"}, Slugs: []string{"weekly-*"}, ExpiresIn: "90d"})
	if err != nil {
		t.Fatal(err)
	}
	key, prefix := issued.Key, issued.APIKey.Prefix
	if open, err := admin.IssueKey(ctx, "ana", api.IssueKeyRequest{}); err != nil || !open.APIKey.ExpiresAt.Equal(now.Add(365*day)) {
		t.Errorf("a key without an expiry under max_key_ttl: %+v, %v", open, err)
	}

	me, err := cli.NewClient("http://api.localhost", key, hc).Me(ctx)
	if err != nil || len(me.Slugs) != 1 || me.Slugs[0] != "weekly-*" || !me.ExpiresAt.Equal(now.Add(90*day)) {
		t.Fatalf("GET /me = %+v, %v", me, err)
	}

	cs := mcpSession(t, addr, key)
	if in := cs.InitializeResult().Instructions; !strings.Contains(in, "restricted to artifacts whose slug matches weekly-*") {
		t.Errorf("instructions do not name the restriction: %s", in)
	}
	file := []map[string]any{{"path": "index.html", "content": "<h1>Week 1</h1>"}}
	if text, failed := toolText(t, cs, "create_artifact", map[string]any{"slug": "weekly-report", "name": "Weekly", "description": "d", "files": file}); failed {
		t.Fatalf("create_artifact inside the restriction: %s", text)
	}
	if text, failed := toolText(t, cs, "create_artifact", map[string]any{"slug": "payroll", "name": "n", "description": "d", "files": file}); !failed ||
		!strings.HasPrefix(text, "forbidden: this credential is restricted to slugs matching weekly-*") {
		t.Errorf("create_artifact outside the restriction: %q", text)
	}
	if text, failed := toolText(t, cs, "set_visibility", map[string]any{"slug": "weekly-report", "visibility": "unlisted"}); !failed ||
		!strings.HasPrefix(text, `forbidden: scope "artifacts:share" required`) {
		t.Errorf("set_visibility without the share scope: %q", text)
	}
	wantStatus(t, get(t, hc, "http://weekly-report.localhost/"), http.StatusOK, "the published page")

	// The same key as an OAuth connection, for a host that speaks only OAuth.
	const redirect = "http://127.0.0.1:9/callback"
	b := newBrowser(t, addr)
	body, _ := json.Marshal(map[string]any{"client_name": "Routine", "redirect_uris": []string{redirect}})
	req, _ := http.NewRequest(http.MethodPost, "http://mcp.localhost/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&client); err != nil || client.ClientID == "" {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	page := b.get("http://mcp.localhost/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode())
	wantStatus(t, page, http.StatusOK, "the consent page")
	_, after, _ := strings.Cut(page.body, `name="request_id" value="`)
	requestID, _, _ := strings.Cut(after, `"`)
	consent := b.send(http.MethodPost, "http://mcp.localhost/oauth/consent",
		url.Values{"request_id": {requestID}, "action": {"approve"}, "api_key": {key}},
		http.Header{"Origin": {"http://mcp.localhost"}})
	loc, err := url.Parse(b.follow(consent, "consent"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("consent redirected to %v (%v)", loc, err)
	}
	first := tokenRequest(t, hc, url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ClientID},
		"redirect_uri": {redirect}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}})
	if first.AccessToken == "" || first.ExpiresIn != 1800 || first.Scope != "artifacts:read artifacts:write" {
		t.Fatalf("token response = %+v, want an access token of auth.tokens.access_ttl with the key's scopes", first)
	}

	viaOAuth, err := cli.NewClient("http://api.localhost", first.AccessToken, hc).Me(ctx)
	if err != nil || viaOAuth.Source != "oauth" || len(viaOAuth.Slugs) != 1 || viaOAuth.Slugs[0] != "weekly-*" ||
		!viaOAuth.ExpiresAt.Equal(now.Add(90*day)) {
		t.Fatalf("GET /me with the connection = %+v, %v; want the key's restriction and expiry", viaOAuth, err)
	}
	oauthCS := mcpSession(t, addr, first.AccessToken)
	if text, failed := toolText(t, oauthCS, "update_artifact", map[string]any{"slug": "weekly-report",
		"files": []map[string]string{{"path": "index.html", "content": "<h1>Weekly, again</h1>"}}}); failed {
		t.Fatalf("update through the connection: %s", text)
	}
	if text, failed := toolText(t, oauthCS, "get_artifact", map[string]any{"slug": "payroll"}); !failed || !strings.HasPrefix(text, "forbidden:") {
		t.Errorf("a read outside the restriction through the connection: %q", text)
	}

	grants, err := admin.ListGrants(ctx, "ana")
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	g := grants[0]
	if g.Client != "Routine" || g.KeyPrefix != prefix || !g.ExpiresAt.Equal(now.Add(45*day)) || !g.CreatedAt.Equal(now) {
		t.Errorf("grant = %+v, want the client's name, the key and auth.tokens.refresh_ttl", g)
	}
	events, err := admin.Audit(ctx, cli.AuditParams{Slug: "weekly-report"})
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]bool{}
	for _, ev := range events.Events {
		credentials[ev.Details["credential"]] = true
	}
	if want := fmt.Sprintf("oauth:%d", g.ID); !credentials[want] || !credentials["api_key:"+prefix] {
		t.Errorf("audit credentials = %v, want %s and api_key:%s: one line per agent", credentials, want, prefix)
	}

	// A lost answer to a refresh does not end the connection.
	refresh := func(token string) tokens {
		return tokenRequest(t, hc, url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ClientID}, "refresh_token": {token}})
	}
	in.clock.Advance(time.Hour)
	if lost := refresh(first.RefreshToken); lost.AccessToken == "" {
		t.Fatalf("refresh: %+v", lost)
	}
	in.clock.Advance(10 * time.Second)
	second := refresh(first.RefreshToken)
	if second.AccessToken == "" {
		t.Fatalf("the same refresh token inside auth.tokens.refresh_reuse_window: %+v", second)
	}
	if grants, err = admin.ListGrants(ctx, "ana"); err != nil || len(grants) != 1 || grants[0].ID != g.ID {
		t.Errorf("grants after the reuse = %+v, %v; want the same one", grants, err)
	}

	// Rotating the key ends the connection that was approved with it.
	rotated, err := admin.RotateKey(ctx, "ana", prefix, "0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.NewClient("http://api.localhost", second.AccessToken, hc).Me(ctx); err == nil {
		t.Error("the connection outlived the key it was approved with")
	}
	if me, err := cli.NewClient("http://api.localhost", rotated.Key, hc).Me(ctx); err != nil || me.Slugs[0] != "weekly-*" {
		t.Errorf("the rotated key: %+v, %v", me, err)
	}
	if err := admin.RevokeGrant(ctx, g.ID); err != nil {
		t.Errorf("revoking the grant by the id of its audit credential: %v", err)
	}
}

// A key string is shown once, in the response that issues it: it must not
// reach a log line or a span, whichever route made or read it.
func TestKeySecretsStayOutOfSpansAndLogs(t *testing.T) {
	var stderr lockedBuffer
	spans := spanSink{tracetest.NewInMemoryExporter()}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, testConfig(t), app.Options{
		Logger:       slog.New(slog.NewJSONHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SpanExporter: spans,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-a.Started()
	hc := client(a.Addrs().HTTP)

	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "ana"}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	issued, err := admin.IssueKey(ctx, "ana", api.IssueKeyRequest{Label: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	keys = append(keys, issued.Key)
	own := cli.NewClient("http://api.localhost", issued.Key, hc)
	child, err := own.IssueMyKey(ctx, api.IssueKeyRequest{Slugs: []string{"weekly-*"}, ExpiresIn: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	keys = append(keys, child.Key)
	rotated, err := own.RotateMyKey(ctx, child.APIKey.Prefix, "1h")
	if err != nil {
		t.Fatal(err)
	}
	keys = append(keys, rotated.Key)
	byAdmin, err := admin.RotateKey(ctx, "ana", rotated.APIKey.Prefix, "0")
	if err != nil {
		t.Fatal(err)
	}
	keys = append(keys, byAdmin.Key)
	if err := own.RevokeMyKey(ctx, byAdmin.APIKey.Prefix); err != nil {
		t.Fatal(err)
	}
	// A revoked key and a malformed one are refused, and still not logged.
	if _, err := cli.NewClient("http://api.localhost", byAdmin.Key, hc).Me(ctx); err == nil {
		t.Error("a revoked key was accepted")
	}
	cs := mcpSession(t, a.Addrs().HTTP, issued.Key)
	if text, failed := toolText(t, cs, "list_artifacts", map[string]any{}); failed {
		t.Fatalf("list_artifacts: %s", text)
	}
	cs.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var seen strings.Builder
	seen.WriteString(stderr.String())
	stubs := spans.GetSpans()
	for _, s := range stubs {
		fmt.Fprintf(&seen, "\n%s %s", s.Name, s.Status.Description)
		for _, kv := range s.Attributes {
			fmt.Fprintf(&seen, " %s=%s", kv.Key, kv.Value.String())
		}
		for _, ev := range s.Events {
			fmt.Fprintf(&seen, " %s", ev.Name)
			for _, kv := range ev.Attributes {
				fmt.Fprintf(&seen, " %s=%s", kv.Key, kv.Value.String())
			}
		}
	}
	text := seen.String()
	for _, want := range []string{"user.issue_key", "user.rotate_key", "user.revoke_key", `"msg":"request"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the spans and logs lack %q, so this test saw too little", want)
		}
	}
	if len(keys) != 4 {
		t.Fatalf("%d keys issued, want 4", len(keys))
	}
	for i, key := range keys {
		secret := key[strings.LastIndex(key, "_")+1:]
		if len(secret) != 52 {
			t.Fatalf("key %d has a secret of %d characters", i, len(secret))
		}
		if strings.Contains(text, secret) {
			t.Errorf("the secret of key %d is in a span or a log line", i)
		}
	}
	if strings.Contains(text, adminToken) {
		t.Error("the admin token is in a span or a log line")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
