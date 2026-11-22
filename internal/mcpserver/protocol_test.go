package mcpserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

// cacheOf is the cache fields of a result, or the error that stopped it.
func cacheOf[R interface {
	GetTTLMs() int
	GetCacheScope() string
}](r R, err error) (mcp.Cacheable, error) {
	if err != nil {
		return mcp.Cacheable{}, err
	}
	return mcp.Cacheable{TTLMs: r.GetTTLMs(), CacheScope: r.GetCacheScope()}, nil
}

func TestCacheable(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "<p>one</p>"}},
	})
	ctx := t.Context()
	read := func(uri string) func() (mcp.Cacheable, error) {
		return func() (mcp.Cacheable, error) {
			return cacheOf(cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}))
		}
	}
	fiveMinutes := mcp.Cacheable{TTLMs: 300000, CacheScope: "private"}
	stale := mcp.Cacheable{CacheScope: "private"}
	tests := []struct {
		name  string
		fetch func() (mcp.Cacheable, error)
		want  mcp.Cacheable
	}{
		{"tools/list", func() (mcp.Cacheable, error) { return cacheOf(cs.ListTools(ctx, nil)) }, fiveMinutes},
		{"prompts/list", func() (mcp.Cacheable, error) { return cacheOf(cs.ListPrompts(ctx, nil)) }, fiveMinutes},
		{"resources/list", func() (mcp.Cacheable, error) { return cacheOf(cs.ListResources(ctx, nil)) }, fiveMinutes},
		{"resources/templates/list", func() (mcp.Cacheable, error) { return cacheOf(cs.ListResourceTemplates(ctx, nil)) }, fiveMinutes},
		{"the skill", read("skill://kazari/SKILL.md"), fiveMinutes},
		{"the preview app", read("ui://kazari/preview.html"), fiveMinutes},
		{"an artifact", read("kazari://artifacts/demo"), stale},
		{"an artifact's file", read("kazari://artifacts/demo/files/index.html"), stale},
	}
	for _, tt := range tests {
		got, err := tt.fetch()
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if diff := cmp.Diff(tt.want, got); diff != "" {
			t.Errorf("%s cache fields (-want +got):\n%s", tt.name, diff)
		}
	}
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
	if err := sc.Err(); err != nil {
		t.Fatalf("read the response: %v", err)
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

	request := func(method, params string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"` + method + `","params":{` + params + meta + `}}`
	}
	call := request("tools/call", `"name":"list_artifacts","arguments":{},`)
	readSkill := request("resources/read", `"uri":"skill://kazari/SKILL.md",`)
	prompt := request("prompts/get", `"name":"publish-page",`)
	for _, tc := range []struct {
		name    string
		body    string
		headers map[string]string
		status  int
	}{
		{"tools/call with matching headers", call, map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "list_artifacts"}, http.StatusOK},
		{"tools/call without Mcp-Method", call, map[string]string{"Mcp-Name": "list_artifacts"}, http.StatusBadRequest},
		{"tools/call with another Mcp-Method", call, map[string]string{"Mcp-Method": "tools/list", "Mcp-Name": "list_artifacts"}, http.StatusBadRequest},
		{"tools/call without Mcp-Name", call, map[string]string{"Mcp-Method": "tools/call"}, http.StatusBadRequest},
		{"tools/call with another Mcp-Name", call, map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "delete_artifact"}, http.StatusBadRequest},
		{"resources/read with matching headers", readSkill, map[string]string{"Mcp-Method": "resources/read", "Mcp-Name": "skill://kazari/SKILL.md"}, http.StatusOK},
		{"resources/read without Mcp-Name", readSkill, map[string]string{"Mcp-Method": "resources/read"}, http.StatusBadRequest},
		{"prompts/get without Mcp-Name", prompt, map[string]string{"Mcp-Method": "prompts/get"}, http.StatusBadRequest},
		{"prompts/get with another Mcp-Name", prompt, map[string]string{"Mcp-Method": "prompts/get", "Mcp-Name": "other"}, http.StatusBadRequest},
	} {
		status, msg := postRPC(t, srv.URL, tc.headers, tc.body)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (%v)", tc.name, status, tc.status, msg)
			continue
		}
		if tc.status == http.StatusOK {
			if msg["result"] == nil {
				t.Errorf("%s: message %v, want a result", tc.name, msg)
			}
			continue
		}
		if e, _ := msg["error"].(map[string]any); e == nil || e["code"] != float64(mcp.CodeHeaderMismatch) {
			t.Errorf("%s: message %v, want error %d", tc.name, msg, mcp.CodeHeaderMismatch)
		}
	}
}
