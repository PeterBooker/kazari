package oauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type metadataServer struct {
	srv *httptest.Server

	mu          sync.Mutex
	fetches     int
	name        string
	clientID    string // "" = the document's own URL
	contentType string
	status      int
}

func newMetadataServer(t *testing.T) *metadataServer {
	m := &metadataServer{name: "Claude Code", contentType: "application/json", status: http.StatusOK}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.fetches++
		id := m.clientID
		if id == "" {
			id = m.srv.URL + r.URL.Path
		}
		w.Header().Set("Content-Type", m.contentType)
		w.WriteHeader(m.status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  id,
			"client_name":                m.name,
			"redirect_uris":              []string{"http://localhost/callback", "http://127.0.0.1/callback"},
			"token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *metadataServer) set(f func(m *metadataServer)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

func (m *metadataServer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fetches
}

func TestClientMetadataDocument(t *testing.T) {
	ctx := context.Background()
	meta := newMetadataServer(t)
	e := newEnv(t, envOptions{httpClient: meta.srv.Client()})
	clientID := meta.srv.URL + "/client.json"
	p := newPKCE("cimd")
	authorize := func() *http.Response {
		t.Helper()
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, "http://127.0.0.1:43210/callback", "", p).Encode())
		return resp
	}

	if loc := location(t, authorize()); loc.Host != "idp.example" {
		t.Fatalf("authorize redirected to %s", loc)
	}
	if n := meta.count(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
	c, err := e.store.GetClient(ctx, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Claude Code" || c.FetchedAt == nil || !c.FetchedAt.Equal(t0) {
		t.Errorf("cached client = %+v", c)
	}

	e.clock.Advance(30 * time.Minute)
	location(t, authorize())
	if n := meta.count(); n != 1 {
		t.Errorf("fetches after 30m = %d, want 1", n)
	}

	meta.set(func(m *metadataServer) { m.name = "Claude Code 2" })
	e.clock.Advance(31 * time.Minute)
	location(t, authorize())
	if n := meta.count(); n != 2 {
		t.Errorf("fetches after 61m = %d, want 2", n)
	}
	c, err = e.store.GetClient(ctx, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Claude Code 2" || !c.FetchedAt.Equal(e.clock.Now()) || !c.CreatedAt.Equal(t0) {
		t.Errorf("refreshed client = %+v", c)
	}

	page := e.signIn(authorizeParams(clientID, "http://localhost:5555/callback", "", p))
	if !strings.Contains(page, "Claude Code 2") {
		t.Errorf("consent page does not name the client:\n%s", page)
	}
	code := e.approve(nil)
	if tr := e.exchange(clientID, "http://localhost:5555/callback", code, p); tr.status != http.StatusOK {
		t.Errorf("exchange = %d %s", tr.status, tr.Error)
	}
}

func TestClientMetadataDocumentFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *metadataServer)
		path  string
	}{
		{"client_id mismatch", func(m *metadataServer) { m.clientID = "https://other.example/client.json" }, "/client.json"},
		{"not found", func(m *metadataServer) { m.status = http.StatusNotFound }, "/client.json"},
		{"not json", func(m *metadataServer) { m.contentType = "text/html" }, "/client.json"},
		{"redirect", func(m *metadataServer) { m.status = http.StatusFound }, "/client.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := newMetadataServer(t)
			meta.set(tt.setup)
			e := newEnv(t, envOptions{httpClient: meta.srv.Client()})
			q := authorizeParams(meta.srv.URL+tt.path, "http://127.0.0.1/callback", "", newPKCE("v"))
			resp, body := e.get("/oauth/authorize?" + q.Encode())
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
				t.Errorf("status = %d, Location %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
			}
		})
	}

	t.Run("token endpoint rejects unfetched client", func(t *testing.T) {
		meta := newMetadataServer(t)
		e := newEnv(t, envOptions{httpClient: meta.srv.Client()})
		tr := e.token(map[string][]string{"grant_type": {"refresh_token"}, "client_id": {meta.srv.URL + "/c.json"}, "refresh_token": {"x"}})
		if tr.status != http.StatusUnauthorized || tr.Error != "invalid_client" {
			t.Errorf("token = %d %q, want 401 invalid_client", tr.status, tr.Error)
		}
	})
}

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

func TestDefaultHTTPClientRefusesLocalAddresses(t *testing.T) {
	meta := newMetadataServer(t)
	var logs lockedBuffer
	e := newEnv(t, envOptions{logger: slog.New(slog.NewTextHandler(&logs, nil))})
	q := authorizeParams(meta.srv.URL+"/client.json", "http://127.0.0.1/callback", "", newPKCE("v"))
	resp, _ := e.get("/oauth/authorize?" + q.Encode())
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if meta.count() != 0 {
		t.Errorf("metadata server was reached %d times", meta.count())
	}
	if !strings.Contains(logs.String(), "non-public address") {
		t.Errorf("log does not explain the refusal:\n%s", logs.String())
	}
}
