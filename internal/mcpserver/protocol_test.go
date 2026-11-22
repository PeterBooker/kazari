package mcpserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func TestCacheable(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "<p>one</p>"}},
	})

	const day = 300000
	check := func(what string, err error, c mcp.Cacheable, ttl int) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
			return
		}
		if c.CacheScope != "private" || c.TTLMs != ttl {
			t.Errorf("%s: cacheScope %q, ttlMs %d; want private, %d", what, c.CacheScope, c.TTLMs, ttl)
		}
	}
	tools, err := cs.ListTools(e.ctx, nil)
	check("tools/list", err, tools.Cacheable, day)
	prompts, err := cs.ListPrompts(e.ctx, nil)
	check("prompts/list", err, prompts.Cacheable, day)
	resources, err := cs.ListResources(e.ctx, nil)
	check("resources/list", err, resources.Cacheable, day)
	templates, err := cs.ListResourceTemplates(e.ctx, nil)
	check("resources/templates/list", err, templates.Cacheable, day)
	skill, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "skill://kazari/SKILL.md"})
	check("resources/read of the skill", err, skill.Cacheable, day)
	record, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo"})
	check("resources/read of an artifact", err, record.Cacheable, 0)
	file, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo/files/index.html"})
	check("resources/read of a file", err, file.Cacheable, 0)
}

// meta is the _meta a 2026-07-28 request carries in place of initialize.
const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientInfo":{"name":"test","version":"v0"},` +
	`"io.modelcontextprotocol/clientCapabilities":{}}`

// postRPC sends body at 2026-07-28 with headers and returns the status and the
// JSON-RPC message, from the body or from its first SSE event.
func postRPC(t *testing.T, url string, headers map[string]string, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msg map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			line = data
		}
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("decode %s: %v", line, err)
			}
			break
		}
	}
	return resp.StatusCode, msg
}

func TestStatelessProtocol(t *testing.T) {
	e := newEnv(t)
	restricted := member
	restricted.Slugs = []string{"weekly-*"}
	e.deps.Auth = auth.AuthenticatorFunc(func(context.Context, string) (auth.Principal, error) { return restricted, nil })
	srv := httptest.NewServer(mcpserver.NewHandler(e.deps, e.opts))
	t.Cleanup(srv.Close)

	status, msg := postRPC(t, srv.URL, map[string]string{"Mcp-Method": "server/discover"},
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
	res, _ := msg["result"].(map[string]any)
	if status != http.StatusOK || res == nil {
		t.Fatalf("server/discover: status %d, message %v", status, msg)
	}
	if res["cacheScope"] != "private" || res["ttlMs"] != 300000.0 {
		t.Errorf("server/discover: cacheScope %v, ttlMs %v; want private, 300000", res["cacheScope"], res["ttlMs"])
	}
	if instr, _ := res["instructions"].(string); !strings.Contains(instr, "weekly-*") {
		t.Errorf("server/discover instructions do not name the credential's slugs: %q", instr)
	}
	if versions, _ := res["supportedVersions"].([]any); len(versions) == 0 || versions[0] != "2026-07-28" {
		t.Errorf("server/discover supportedVersions = %v, want 2026-07-28 first", res["supportedVersions"])
	}

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_artifacts","arguments":{},` + meta + `}}`
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"matching headers", map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "list_artifacts"}, http.StatusOK},
		{"no Mcp-Method", map[string]string{"Mcp-Name": "list_artifacts"}, http.StatusBadRequest},
		{"another Mcp-Method", map[string]string{"Mcp-Method": "tools/list", "Mcp-Name": "list_artifacts"}, http.StatusBadRequest},
		{"no Mcp-Name", map[string]string{"Mcp-Method": "tools/call"}, http.StatusBadRequest},
		{"another Mcp-Name", map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "delete_artifact"}, http.StatusBadRequest},
	} {
		status, msg := postRPC(t, srv.URL, tc.headers, call)
		if status != tc.status {
			t.Errorf("tools/call with %s: status %d, want %d (%v)", tc.name, status, tc.status, msg)
			continue
		}
		if tc.status == http.StatusBadRequest {
			if e, _ := msg["error"].(map[string]any); e == nil || e["code"] != float64(mcp.CodeHeaderMismatch) {
				t.Errorf("tools/call with %s: message %v, want error %d", tc.name, msg, mcp.CodeHeaderMismatch)
			}
		}
	}
}
