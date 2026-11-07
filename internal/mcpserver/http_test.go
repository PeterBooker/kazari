package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

const metadataURL = "https://mcp.art.test/.well-known/oauth-protected-resource"

func newHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	e := newEnv(t)
	e.deps.Auth = auth.AuthenticatorFunc(func(_ context.Context, token string) (auth.Principal, error) {
		if token == "good" {
			return member, nil
		}
		return auth.Principal{}, auth.ErrInvalidToken
	})
	e.opts.ResourceMetadataURL = metadataURL
	e.opts.AllowedOrigins = []string{"https://claude.ai"}
	srv := httptest.NewServer(mcpserver.NewHandler(e.deps, e.opts))
	t.Cleanup(srv.Close)
	return srv
}

const listToolsBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

func post(t *testing.T, url string, header map[string]string) (*http.Response, api.ErrorResponse) {
	t.Helper()
	return postBody(t, url, listToolsBody, header)
}

func postBody(t *testing.T, url, payload string, header map[string]string) (*http.Response, api.ErrorResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// SDK answers are JSON-RPC, not api.ErrorResponse, and leave body zero.
	var body api.ErrorResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(resp.Body).Decode(&body)
	}
	return resp, body
}

func TestHTTPUnauthorized(t *testing.T) {
	srv := newHTTPServer(t)
	for _, h := range []map[string]string{nil, {"Authorization": "Bearer wrong"}} {
		resp, body := post(t, srv.URL, h)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+metadataURL+`"`) {
			t.Errorf("WWW-Authenticate = %q", got)
		}
		if body.Error.Code != api.CodeUnauthorized {
			t.Errorf("body = %+v", body)
		}
	}
}

func TestHTTPOrigin(t *testing.T) {
	srv := newHTTPServer(t)
	resp, body := post(t, srv.URL, map[string]string{"Authorization": "Bearer good", "Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden || body.Error.Code != api.CodeForbidden {
		t.Errorf("bad origin: status %d, body %+v", resp.StatusCode, body)
	}
	if resp, _ := post(t, srv.URL, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad origin without token: status %d", resp.StatusCode)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, origin := range []string{"https://claude.ai", "https://" + host} {
		if resp, _ := post(t, srv.URL, map[string]string{"Authorization": "Bearer good", "Origin": origin}); resp.StatusCode != http.StatusOK {
			t.Errorf("origin %s: status %d, want 200", origin, resp.StatusCode)
		}
	}
}

func TestHTTPProtocolVersion(t *testing.T) {
	srv := newHTTPServer(t)
	resp, body := post(t, srv.URL, map[string]string{"Authorization": "Bearer good", "MCP-Protocol-Version": "1999-01-01"})
	if resp.StatusCode != http.StatusBadRequest || body.Error.Code != api.CodeInvalidArgument {
		t.Errorf("status %d, body %+v", resp.StatusCode, body)
	}
	for _, v := range mcp.SupportedProtocolVersions() {
		if _, body := post(t, srv.URL, map[string]string{"Authorization": "Bearer good", "MCP-Protocol-Version": v}); body.Error.Code != "" {
			t.Errorf("version %s rejected: %+v", v, body)
		}
	}
}

// TestHTTPBatchRefused pins one call per POST: the per-IP limit charges a POST
// once, and the SDK would run every call of a batch on its own goroutine.
func TestHTTPBatchRefused(t *testing.T) {
	srv := newHTTPServer(t)
	good := map[string]string{"Authorization": "Bearer good"}
	batch := "[" + listToolsBody + "," + listToolsBody + "]"

	for name, c := range map[string]struct {
		body   string
		header map[string]string
	}{
		"no version header":        {batch, good},
		"one call":                 {"[" + listToolsBody + "]", good},
		"legacy version":           {batch, map[string]string{"Authorization": "Bearer good", "MCP-Protocol-Version": "2025-03-26"}},
		"leading whitespace":       {" \t\r\n" + batch, good},
		"whitespace past a buffer": {strings.Repeat(" ", 64<<10) + batch, good},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := postBody(t, srv.URL, c.body, c.header)
			if resp.StatusCode != http.StatusBadRequest || body.Error.Code != api.CodeInvalidArgument {
				t.Errorf("status %d, body %+v", resp.StatusCode, body)
			}
		})
	}

	if resp, _ := postBody(t, srv.URL, batch, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("batch without a token: status %d", resp.StatusCode)
	}
	for _, body := range []string{listToolsBody, " \n" + listToolsBody, strings.Repeat(" ", 64<<10) + listToolsBody} {
		if resp, out := postBody(t, srv.URL, body, good); resp.StatusCode != http.StatusOK || out.Error.Code != "" {
			t.Errorf("single message of %d bytes: status %d, %+v", len(body), resp.StatusCode, out)
		}
	}
}

// TestHTTPBodyLimitCoversWhitespace: the batch check must not read past the body limit.
func TestHTTPBodyLimitCoversWhitespace(t *testing.T) {
	e := newEnv(t)
	e.deps.Auth = auth.AuthenticatorFunc(func(context.Context, string) (auth.Principal, error) { return member, nil })
	e.opts.MaxVersionBytes = 1
	srv := httptest.NewServer(mcpserver.NewHandler(e.deps, e.opts))
	t.Cleanup(srv.Close)
	body := strings.Repeat(" ", 4<<20) + listToolsBody
	if resp, _ := postBody(t, srv.URL, body, map[string]string{"Authorization": "Bearer good"}); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d", resp.StatusCode)
	}
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func TestHTTPClient(t *testing.T) {
	srv := newHTTPServer(t)
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           &http.Client{Transport: bearer{"good", http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 19 {
		t.Errorf("%d tools, want 19", len(tools.Tools))
	}
	pub, _ := ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "over-http", "name": "x", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "hi"}},
	})
	if pub.Artifact.Owner != "alice" || pub.URL != "https://over-http.art.test/" {
		t.Errorf("create over HTTP = %+v", pub)
	}
}

func TestHTTPHardDeleteConfirmation(t *testing.T) {
	srv := newHTTPServer(t)
	ctx := context.Background()
	connect := func(version string, asked *int) *mcp.ClientSession {
		t.Helper()
		transport := &mcp.StreamableClientTransport{
			Endpoint:             srv.URL,
			HTTPClient:           &http.Client{Transport: bearer{"good", http.DefaultTransport}},
			DisableStandaloneSSE: true,
			MaxRetries:           -1,
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, &mcp.ClientOptions{
			ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				*asked++
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			},
		})
		cs, err := c.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	for _, c := range []struct {
		version string
		asks    int
	}{{"2026-07-28", 1}, {"2025-11-25", 0}} {
		var asked int
		cs := connect(c.version, &asked)
		ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "purge-me", "name": "x", "description": "x",
			"files": []map[string]any{{"path": "index.html", "content": "hi"}}})
		out, res := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "purge-me", "hard": true})
		if !out.OK || firstLine(res) != "Purged purge-me permanently." || asked != c.asks {
			t.Errorf("%s: purge = %+v %q, asked %d times, want %d", c.version, out, firstLine(res), asked, c.asks)
		}
		// The purge tombstone blocks the slug for the next round.
		srv = newHTTPServer(t)
	}
}
