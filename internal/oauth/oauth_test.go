package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"agalma/internal/auth"
	"agalma/internal/oauth"
	"agalma/internal/user"
)

func TestMetadata(t *testing.T) {
	e := newEnv(t, envOptions{})
	tests := []struct {
		path string
		want map[string]any
	}{
		{"/.well-known/oauth-authorization-server", map[string]any{
			"issuer":                                e.srv.URL,
			"authorization_endpoint":                e.srv.URL + "/oauth/authorize",
			"token_endpoint":                        e.srv.URL + "/oauth/token",
			"registration_endpoint":                 e.srv.URL + "/oauth/register",
			"scopes_supported":                      []any{"artifacts:read", "artifacts:write", "artifacts:delete", "admin"},
			"response_types_supported":              []any{"code"},
			"response_modes_supported":              []any{"query"},
			"grant_types_supported":                 []any{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []any{"none"},
			"code_challenge_methods_supported":      []any{"S256"},
			"client_id_metadata_document_supported": true,
		}},
		{"/.well-known/oauth-protected-resource", map[string]any{
			"resource":                 e.srv.URL + "/",
			"authorization_servers":    []any{e.srv.URL},
			"scopes_supported":         []any{"artifacts:read", "artifacts:write", "artifacts:delete", "admin"},
			"bearer_methods_supported": []any{"header"},
		}},
	}
	for _, tt := range tests {
		for _, path := range []string{tt.path, tt.path + "/mcp"} {
			t.Run(path, func(t *testing.T) {
				resp, body := e.get(path)
				if strings.HasPrefix(path, "/.well-known/oauth-authorization-server/") {
					if resp.StatusCode != http.StatusNotFound {
						t.Errorf("status = %d, want 404", resp.StatusCode)
					}
					return
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d: %s", resp.StatusCode, body)
				}
				if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
					t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tt.want, got); diff != "" {
					t.Errorf("metadata (-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestRegister(t *testing.T) {
	e := newEnv(t, envOptions{})
	tests := []struct {
		name      string
		body      string
		status    int
		wantError string
	}{
		{"https and loopback", `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback","http://localhost/callback","http://127.0.0.1:3000/cb","http://[::1]/cb"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`, 201, ""},
		{"no auth method", `{"redirect_uris":["https://app.example/cb"]}`, 201, ""},
		{"missing redirect_uris", `{"client_name":"x"}`, 400, "invalid_redirect_uri"},
		{"plain http", `{"redirect_uris":["http://app.example/cb"]}`, 400, "invalid_redirect_uri"},
		{"custom scheme", `{"redirect_uris":["myapp://cb"]}`, 400, "invalid_redirect_uri"},
		{"fragment", `{"redirect_uris":["https://app.example/cb#x"]}`, 400, "invalid_redirect_uri"},
		{"relative", `{"redirect_uris":["/cb"]}`, 400, "invalid_redirect_uri"},
		{"confidential client", `{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, 400, "invalid_client_metadata"},
		{"implicit grant", `{"redirect_uris":["https://app.example/cb"],"grant_types":["implicit"]}`, 400, "invalid_client_metadata"},
		{"not json", `redirect_uris=x`, 400, "invalid_client_metadata"},
		{"too large", `{"client_name":"` + strings.Repeat("a", 70<<10) + `","redirect_uris":["https://app.example/cb"]}`, 413, "invalid_client_metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, e.url("/oauth/register"), strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			resp, body := e.do(req)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.status, body)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" {
				if got["error"] != tt.wantError {
					t.Errorf("error = %v, want %s", got["error"], tt.wantError)
				}
				return
			}
			id, _ := got["client_id"].(string)
			if !strings.HasPrefix(id, "dcr_") || len(id) != len("dcr_")+24 {
				t.Errorf("client_id = %q, want dcr_ + 24 characters", id)
			}
			if got["token_endpoint_auth_method"] != "none" {
				t.Errorf("token_endpoint_auth_method = %v", got["token_endpoint_auth_method"])
			}
			c, err := e.store.GetClient(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			var sent struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			_ = json.Unmarshal([]byte(tt.body), &sent)
			if diff := cmp.Diff(sent.RedirectURIs, c.RedirectURIs); diff != "" {
				t.Errorf("stored redirect URIs (-want +got):\n%s", diff)
			}
			if c.FetchedAt != nil {
				t.Errorf("DCR client has FetchedAt %v", c.FetchedAt)
			}
		})
	}
}

func TestAuthorizeClientErrorsRenderHTML(t *testing.T) {
	e := newEnv(t, envOptions{})
	clientID := e.register("https://app.example/cb")
	p := newPKCE("v")
	tests := []struct {
		name   string
		params url.Values
	}{
		{"unknown client", authorizeParams("dcr_nope", "https://app.example/cb", "", p)},
		{"missing client", authorizeParams("", "https://app.example/cb", "", p)},
		{"unregistered redirect", authorizeParams(clientID, "https://evil.example/cb", "", p)},
		{"redirect with other port", authorizeParams(clientID, "https://app.example:8443/cb", "", p)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := e.get("/oauth/authorize?" + tt.params.Encode())
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				t.Errorf("redirected to %s", loc)
			}
			checkPageHeaders(t, resp)
			if !strings.Contains(body, "<html") {
				t.Errorf("body is not an HTML page: %s", body)
			}
		})
	}
}

// checkPageHeaders asserts the security headers every HTML page carries.
func checkPageHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	for k, want := range map[string]string{
		"Content-Type":    "text/html; charset=utf-8",
		"Cache-Control":   "no-store",
		"X-Frame-Options": "DENY",
		"Referrer-Policy": "no-referrer",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

func TestAuthorizeErrorsRedirect(t *testing.T) {
	e := newEnv(t, envOptions{})
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("v")
	with := func(k, v string) url.Values {
		q := authorizeParams(clientID, redirect, "", p)
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
		return q
	}
	tests := []struct {
		name   string
		params url.Values
		want   string
	}{
		{"response type token", with("response_type", "token"), "unsupported_response_type"},
		{"missing response type", with("response_type", ""), "unsupported_response_type"},
		{"missing challenge", with("code_challenge", ""), "invalid_request"},
		{"plain method", with("code_challenge_method", "plain"), "invalid_request"},
		{"missing method", with("code_challenge_method", ""), "invalid_request"},
		{"short challenge", with("code_challenge", "abc"), "invalid_request"},
		{"unknown scope", with("scope", "artifacts:read nope"), "invalid_scope"},
		{"other resource", with("resource", "https://elsewhere.example/"), "invalid_target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := e.get("/oauth/authorize?" + tt.params.Encode())
			loc := location(t, resp)
			if got := loc.Scheme + "://" + loc.Host + loc.Path; got != redirect {
				t.Errorf("redirected to %s, want %s", got, redirect)
			}
			q := loc.Query()
			if q.Get("error") != tt.want || q.Get("state") != "client-state" || q.Get("error_description") == "" {
				t.Errorf("redirect query = %v, want error %s with state", q, tt.want)
			}
		})
	}
}

func TestAuthorizeAccepts(t *testing.T) {
	e := newEnv(t, envOptions{})
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("v")
	tests := []struct {
		name  string
		tweak func(url.Values)
	}{
		{"resource with slash", func(q url.Values) { q.Set("resource", e.srv.URL+"/") }},
		{"resource without slash", func(q url.Values) { q.Set("resource", e.srv.URL) }},
		{"single redirect omitted", func(q url.Values) { q.Del("redirect_uri") }},
		{"no state", func(q url.Values) { q.Del("state") }},
		{"subset of scopes", func(q url.Values) { q.Set("scope", "artifacts:write artifacts:read") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeParams(clientID, redirect, "", p)
			tt.tweak(q)
			resp, body := e.get("/oauth/authorize?" + q.Encode())
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			loc := location(t, resp)
			if loc.Host != "idp.example" || loc.Query().Get("state") == "" || loc.Query().Get("nonce") == "" ||
				len(loc.Query().Get("code_challenge")) != 43 {
				t.Errorf("redirect = %s, want the identity provider with state, nonce and challenge", loc)
			}
		})
	}
}

func TestRequestCookie(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		e := newEnv(t, envOptions{})
		srv := e.svc
		if !insecure {
			srv = oauth.New(e.store, e.users, nil, e.idp, nil, oauth.Options{Issuer: e.srv.URL, Resource: e.srv.URL, Now: e.clock.Now})
		}
		clientID := e.register("https://app.example/cb")
		rec := &responseRecorder{header: http.Header{}}
		req, _ := http.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")).Encode(), nil)
		srv.Handler().ServeHTTP(rec, req)
		ck := rec.header.Get("Set-Cookie")
		for _, want := range []string{oauth.CookieName + "=", "Path=/oauth", "Max-Age=600", "HttpOnly", "SameSite=Lax"} {
			if !strings.Contains(ck, want) {
				t.Errorf("Set-Cookie %q lacks %q", ck, want)
			}
		}
		if got := strings.Contains(ck, "Secure"); got == insecure {
			t.Errorf("insecure=%v: Set-Cookie %q", insecure, ck)
		}
	}
}

// responseRecorder captures headers without httptest (to stay on the
// public surface it is just enough of an http.ResponseWriter).
type responseRecorder struct {
	header http.Header
	status int
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *responseRecorder) WriteHeader(s int)           { r.status = s }

func TestFullFlow(t *testing.T) {
	tests := []struct {
		name       string
		identity   auth.Identity
		wantHandle string
		wantAdmin  bool
		scope      string
		wantScopes []string
	}{
		{"member gets member scopes", auth.Identity{Issuer: "https://idp.example", Subject: "a", Email: "alice@example.com", EmailVerified: true},
			"alice", false, "", auth.MemberScopes},
		{"member asking for admin is capped", auth.Identity{Issuer: "https://idp.example", Subject: "a", Email: "alice@example.com", EmailVerified: true},
			"alice", false, "artifacts:read admin", []string{auth.ScopeRead}},
		{"admin gets every scope", auth.Identity{Issuer: "https://idp.example", Subject: "b", Email: "boss@example.com", EmailVerified: true},
			"boss", true, "", auth.AllScopes},
		{"narrow request", auth.Identity{Issuer: "https://idp.example", Subject: "b", Email: "boss@example.com", EmailVerified: true},
			"boss", false, "artifacts:read", []string{auth.ScopeRead}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			e.idp.setIdentity(tt.identity)
			const redirect = "https://app.example/cb"
			clientID := e.register(redirect)
			p := newPKCE("verifier")

			page := e.signIn(authorizeParams(clientID, redirect, tt.scope, p))
			for _, want := range append([]string{"Test Client", tt.wantHandle, `name="action" value="approve"`}, tt.wantScopes...) {
				if !strings.Contains(page, want) {
					t.Errorf("consent page lacks %q", want)
				}
			}
			if strings.Contains(page, `name="api_key"`) {
				t.Error("consent page after sign-in asks for an API key")
			}

			code := e.approve(nil)
			if code == "" {
				t.Fatal("no code")
			}
			if e.requestCookie() != "" {
				t.Error("request cookie not cleared")
			}
			tr := e.exchange(clientID, redirect, code, p)
			if tr.status != http.StatusOK {
				t.Fatalf("token: %d %s %s", tr.status, tr.Error, tr.ErrorDescription)
			}
			if tr.TokenType != "Bearer" || tr.ExpiresIn != 3600 || tr.RefreshToken == "" || tr.Scope != strings.Join(tt.wantScopes, " ") {
				t.Errorf("token response = %+v", tr)
			}
			if got := tr.header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}

			p2, err := e.svc.Authenticate(context.Background(), tr.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			want := auth.Principal{Handle: tt.wantHandle, Admin: tt.wantAdmin, Scopes: tt.wantScopes, Source: auth.SourceOAuth, KeyID: p2.KeyID}
			if diff := cmp.Diff(want, p2); diff != "" {
				t.Errorf("principal (-want +got):\n%s", diff)
			}
			if !strings.HasPrefix(p2.KeyID, "oauth:") {
				t.Errorf("KeyID = %q", p2.KeyID)
			}
			if _, err := e.svc.Authenticate(context.Background(), tr.RefreshToken); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("Authenticate(refresh token) err = %v, want ErrInvalidToken", err)
			}

			grants := e.rec.actions(oauth.ActionGrant)
			if len(grants) != 1 || grants[0].Actor != tt.wantHandle || grants[0].Details["client_id"] != clientID ||
				grants[0].Details["scopes"] != strings.Join(tt.wantScopes, " ") {
				t.Errorf("grant audit = %+v", grants)
			}
		})
	}
}

func TestAuthenticateUnknownTokens(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, tok := range []string{"", "agk_abcdefgh_x", strings.Repeat("a", 43)} {
		if _, err := e.svc.Authenticate(context.Background(), tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("Authenticate(%q) err = %v, want ErrInvalidToken", tok, err)
		}
	}
}

func TestDeny(t *testing.T) {
	e := newEnv(t, envOptions{})
	clientID := e.register("https://app.example/cb")
	e.signIn(authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")))
	resp, _ := e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"deny"}})
	q := location(t, resp).Query()
	if q.Get("error") != "access_denied" || q.Get("state") != "client-state" || q.Get("code") != "" {
		t.Errorf("deny redirect query = %v", q)
	}
	if len(e.rec.actions(oauth.ActionGrant)) != 0 {
		t.Error("deny recorded a grant")
	}
}

func TestCallbackFailures(t *testing.T) {
	const redirect = "https://app.example/cb"
	t.Run("wrong state", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		location(t, resp)
		resp, body := e.get("/oauth/callback?code=upstream-code&state=forged")
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("status = %d, Location %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		checkPageHeaders(t, resp)
	})
	t.Run("no cookie", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		resp, _ := e.get("/oauth/callback?code=upstream-code&state=x")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("expired request", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		state := location(t, resp).Query().Get("state")
		e.clock.Advance(11 * time.Minute)
		resp, _ = e.get("/oauth/callback?code=upstream-code&state=" + url.QueryEscape(state))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("not provisioned", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		e.idp.setIdentity(auth.Identity{Issuer: "https://idp.example", Subject: "m", Email: "mallory@other.org", EmailVerified: true})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		state := location(t, resp).Query().Get("state")
		resp, body := e.get("/oauth/callback?code=upstream-code&state=" + url.QueryEscape(state))
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "administrator") {
			t.Errorf("status = %d: %s", resp.StatusCode, body)
		}
		checkPageHeaders(t, resp)
	})
	t.Run("upstream error", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		state := location(t, resp).Query().Get("state")
		resp, _ = e.get("/oauth/callback?" + url.Values{"error": {"access_denied"}, "state": {state}}.Encode())
		if q := location(t, resp).Query(); q.Get("error") != "access_denied" || q.Get("state") != "client-state" {
			t.Errorf("redirect query = %v", q)
		}
	})
	t.Run("bad upstream code", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		state := location(t, resp).Query().Get("state")
		resp, _ = e.get("/oauth/callback?" + url.Values{"code": {"wrong"}, "state": {state}}.Encode())
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
	t.Run("callback replay", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("v")).Encode())
		state := location(t, resp).Query().Get("state")
		cb := "/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {state}}.Encode()
		if resp, _ := e.get(cb); resp.StatusCode != http.StatusOK {
			t.Fatalf("first callback status = %d", resp.StatusCode)
		}
		if resp, _ := e.get(cb); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("second callback status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestConsentCSRF(t *testing.T) {
	e := newEnv(t, envOptions{})
	clientID := e.register("https://app.example/cb")
	e.signIn(authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")))
	real := e.requestCookie()

	tests := []struct {
		name      string
		requestID string
		cookie    bool
	}{
		{"other request id", "forged-request-id", true},
		{"empty request id", "", true},
		{"no cookie", real, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := url.Values{"request_id": {tt.requestID}, "action": {"approve"}}
			req, _ := http.NewRequest(http.MethodPost, e.url("/oauth/consent"), strings.NewReader(v.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.cookie {
				req.AddCookie(&http.Cookie{Name: oauth.CookieName, Value: real})
			}
			resp, err := (&http.Client{CheckRedirect: e.client.CheckRedirect}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
				t.Errorf("status = %d, Location %q, want 403", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
	// The real request is untouched and can still be approved.
	if code := e.approve(nil); code == "" {
		t.Error("approve after rejected posts gave no code")
	}
}

func TestTokenErrors(t *testing.T) {
	const redirect = "https://app.example/cb"
	p := newPKCE("verifier")
	setup := func(t *testing.T) (*env, string, string) {
		e := newEnv(t, envOptions{})
		clientID := e.register(redirect)
		e.signIn(authorizeParams(clientID, redirect, "", p))
		return e, clientID, e.approve(nil)
	}
	tests := []struct {
		name       string
		params     func(clientID, code string) url.Values
		advance    time.Duration
		wantStatus int
		wantError  string
	}{
		{"wrong verifier", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {newPKCE("other").verifier}}
		}, 0, 400, "invalid_grant"},
		{"wrong redirect", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {"https://app.example/other"}, "code": {code}, "code_verifier": {p.verifier}}
		}, 0, 400, "invalid_grant"},
		{"unknown code", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {redirect}, "code": {"nope"}, "code_verifier": {p.verifier}}
		}, 0, 400, "invalid_grant"},
		{"expired code", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {p.verifier}}
		}, 6 * time.Minute, 400, "invalid_grant"},
		{"missing verifier", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {redirect}, "code": {code}}
		}, 0, 400, "invalid_request"},
		{"missing redirect", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "code": {code}, "code_verifier": {p.verifier}}
		}, 0, 400, "invalid_request"},
		{"missing client", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {p.verifier}}
		}, 0, 401, "invalid_client"},
		{"unknown client", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {"dcr_nope"}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {p.verifier}}
		}, 0, 401, "invalid_client"},
		{"unsupported grant", func(c, code string) url.Values {
			return url.Values{"grant_type": {"password"}, "client_id": {c}}
		}, 0, 400, "unsupported_grant_type"},
		{"bad resource", func(c, code string) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "client_id": {c}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {p.verifier}, "resource": {"https://x.example/"}}
		}, 0, 400, "invalid_target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, clientID, code := setup(t)
			e.clock.Advance(tt.advance)
			tr := e.token(tt.params(clientID, code))
			if tr.status != tt.wantStatus || tr.Error != tt.wantError {
				t.Errorf("token = %d %q (%s), want %d %q", tr.status, tr.Error, tr.ErrorDescription, tt.wantStatus, tt.wantError)
			}
			if tt.wantStatus == http.StatusUnauthorized && !strings.HasPrefix(tr.header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("WWW-Authenticate = %q", tr.header.Get("WWW-Authenticate"))
			}
			if tr.header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", tr.header.Get("Cache-Control"))
			}
		})
	}

	t.Run("other client", func(t *testing.T) {
		e, _, code := setup(t)
		other := e.register(redirect)
		if tr := e.exchange(other, redirect, code, p); tr.Error != "invalid_grant" {
			t.Errorf("token = %d %q, want invalid_grant", tr.status, tr.Error)
		}
	})
	t.Run("code reuse", func(t *testing.T) {
		e, clientID, code := setup(t)
		if tr := e.exchange(clientID, redirect, code, p); tr.status != http.StatusOK {
			t.Fatalf("first exchange: %d %s", tr.status, tr.Error)
		}
		if tr := e.exchange(clientID, redirect, code, p); tr.status != http.StatusBadRequest || tr.Error != "invalid_grant" {
			t.Errorf("second exchange = %d %q, want invalid_grant", tr.status, tr.Error)
		}
	})
	t.Run("failed attempt burns the code", func(t *testing.T) {
		e, clientID, code := setup(t)
		if tr := e.exchange(clientID, redirect, code, newPKCE("other")); tr.Error != "invalid_grant" {
			t.Fatalf("wrong verifier = %q", tr.Error)
		}
		if tr := e.exchange(clientID, redirect, code, p); tr.Error != "invalid_grant" {
			t.Errorf("retry with right verifier = %d %q, want invalid_grant", tr.status, tr.Error)
		}
	})
}

func TestRefreshRotation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, first := e.flow("")

	second := e.refresh(clientID, first.RefreshToken, "")
	if second.status != http.StatusOK || second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatalf("refresh = %+v", second)
	}
	if second.Scope != strings.Join(auth.MemberScopes, " ") {
		t.Errorf("refreshed scope = %q", second.Scope)
	}
	if _, err := e.svc.Authenticate(ctx, second.AccessToken); err != nil {
		t.Errorf("new access token: %v", err)
	}

	// Narrowing works; widening does not.
	third := e.refresh(clientID, second.RefreshToken, "artifacts:read")
	if third.status != http.StatusOK || third.Scope != "artifacts:read" {
		t.Fatalf("narrowing refresh = %d %q %q", third.status, third.Error, third.Scope)
	}
	if tr := e.refresh(clientID, third.RefreshToken, "artifacts:write"); tr.Error != "invalid_scope" {
		t.Errorf("widening refresh = %d %q, want invalid_scope", tr.status, tr.Error)
	}
	if tr := e.refresh(e.register("https://app.example/cb"), third.RefreshToken, ""); tr.Error != "invalid_grant" {
		t.Errorf("refresh by another client = %q, want invalid_grant", tr.Error)
	}
	if tr := e.refresh(clientID, third.AccessToken, ""); tr.Error != "invalid_grant" {
		t.Errorf("refresh with an access token = %q, want invalid_grant", tr.Error)
	}

	// Replaying a rotated refresh token revokes the whole family.
	if tr := e.refresh(clientID, first.RefreshToken, ""); tr.status != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Errorf("replay = %d %q, want invalid_grant", tr.status, tr.Error)
	}
	if tr := e.refresh(clientID, third.RefreshToken, ""); tr.Error != "invalid_grant" {
		t.Errorf("latest refresh token after replay = %d %q, want invalid_grant", tr.status, tr.Error)
	}
	for _, tok := range []string{first.AccessToken, second.AccessToken, third.AccessToken} {
		if _, err := e.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("access token after replay: err = %v, want ErrInvalidToken", err)
		}
	}
}

func TestTokenExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, tr := e.flow("")

	e.clock.Advance(time.Hour)
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("access token after 1h: err = %v, want ErrInvalidToken", err)
	}
	next := e.refresh(clientID, tr.RefreshToken, "")
	if next.status != http.StatusOK {
		t.Fatalf("refresh after 1h = %d %s", next.status, next.Error)
	}

	e.clock.Advance(30 * 24 * time.Hour)
	if r := e.refresh(clientID, next.RefreshToken, ""); r.Error != "invalid_grant" {
		t.Errorf("refresh after 30 days = %d %q, want invalid_grant", r.status, r.Error)
	}
}

func TestDisabledUser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, tr := e.flow("")
	disabled := true
	if _, err := e.users.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Authenticate for disabled user: err = %v", err)
	}
	if r := e.refresh(clientID, tr.RefreshToken, ""); r.Error != "invalid_grant" {
		t.Errorf("refresh for disabled user = %q, want invalid_grant", r.Error)
	}
}

func TestLoopbackRedirect(t *testing.T) {
	e := newEnv(t, envOptions{})
	tests := []struct {
		name       string
		registered string
		requested  string
		ok         bool
	}{
		{"localhost any port", "http://localhost/callback", "http://localhost:54321/callback", true},
		{"ipv4 any port", "http://127.0.0.1/callback", "http://127.0.0.1:8080/callback", true},
		{"ipv6 any port", "http://[::1]/callback", "http://[::1]:9000/callback", true},
		{"registered port ignored", "http://127.0.0.1:3000/callback", "http://127.0.0.1:4000/callback", true},
		{"other path", "http://127.0.0.1/callback", "http://127.0.0.1:8080/other", false},
		{"other loopback host", "http://127.0.0.1/callback", "http://localhost:8080/callback", false},
		{"https on loopback is exact", "https://localhost/callback", "https://localhost:8443/callback", false},
		{"non-loopback port", "https://app.example/cb", "https://app.example:8443/cb", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientID := e.register(tt.registered)
			p := newPKCE("loop")
			resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, tt.requested, "", p).Encode())
			if !tt.ok {
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("status = %d, want 400", resp.StatusCode)
				}
				return
			}
			loc := location(t, resp)
			resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {loc.Query().Get("state")}}.Encode())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("callback = %d: %s", resp.StatusCode, body)
			}
			code := e.approve(nil)
			if tr := e.exchange(clientID, tt.requested, code, p); tr.status != http.StatusOK {
				t.Errorf("exchange = %d %s", tr.status, tr.Error)
			}
		})
	}
}

func TestAPIKeyFallback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{noIDP: true})
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol", Role: user.RoleMember}); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.users.IssueKey(ctx, "carol", "laptop", []string{auth.ScopeRead, auth.ScopeWrite}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "http://localhost/callback"
	clientID := e.register(redirect)
	p := newPKCE("apikey")

	page := e.signIn(authorizeParams(clientID, "http://localhost:7777/callback", "", p))
	if !strings.Contains(page, `name="api_key"`) {
		t.Fatalf("consent page has no API key field:\n%s", page)
	}

	// A bad key re-renders the page with an error.
	resp, body := e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}, "api_key": {"agk_wrong"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "not accepted") || !strings.Contains(body, `name="api_key"`) {
		t.Errorf("bad key: status %d:\n%s", resp.StatusCode, body)
	}
	checkPageHeaders(t, resp)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, " http://localhost:7777") {
		t.Errorf("consent CSP = %q, want the redirect origin in form-action", csp)
	}

	code := e.approve(url.Values{"api_key": {key}})
	tr := e.exchange(clientID, "http://localhost:7777/callback", code, p)
	if tr.status != http.StatusOK || tr.Scope != "artifacts:read artifacts:write" {
		t.Fatalf("exchange = %d %q scope %q", tr.status, tr.Error, tr.Scope)
	}
	pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Handle != "carol" || !cmp.Equal(pr.Scopes, []string{auth.ScopeRead, auth.ScopeWrite}) {
		t.Errorf("principal = %+v", pr)
	}
	if g := e.rec.actions(oauth.ActionGrant); len(g) != 1 || g[0].Details["via"] != "api_key" {
		t.Errorf("grant audit = %+v", g)
	}
}

func TestAPIKeyRefusedWithIdentityProvider(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.users.IssueKey(ctx, "carol", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientID := e.register("https://app.example/cb")
	resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")).Encode())
	location(t, resp) // at the identity provider, not signed in yet
	resp, _ = e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}, "api_key": {key}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestGrants(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientA, a := e.flow("")
	e.clock.Advance(time.Minute)
	_, b := e.flow("artifacts:read")

	grants, err := e.svc.ListGrants(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("ListGrants = %d grants, want 2", len(grants))
	}
	newest := grants[0]
	if newest.Kind != oauth.KindRefresh || newest.UserHandle != "alice" || !cmp.Equal(newest.Scopes, []string{auth.ScopeRead}) ||
		!newest.ExpiresAt.Equal(e.clock.Now().Add(30*24*time.Hour)) {
		t.Errorf("newest grant = %+v", newest)
	}

	older := grants[1]
	if older.ClientID != clientA {
		t.Errorf("older grant client = %q, want %q", older.ClientID, clientA)
	}
	if err := e.svc.RevokeGrant(ctx, older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, a.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("access token of revoked grant: err = %v", err)
	}
	if r := e.refresh(clientA, a.RefreshToken, ""); r.Error != "invalid_grant" {
		t.Errorf("refresh of revoked grant = %q", r.Error)
	}
	if _, err := e.svc.Authenticate(ctx, b.AccessToken); err != nil {
		t.Errorf("other grant was revoked too: %v", err)
	}
	if err := e.svc.RevokeGrant(ctx, 999999); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("RevokeGrant(unknown) err = %v, want ErrNotFound", err)
	}

	if err := e.svc.RevokeAllForUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, b.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("access token after RevokeAllForUser: err = %v", err)
	}
	if grants, _ := e.svc.ListGrants(ctx, "alice"); len(grants) != 0 {
		t.Errorf("ListGrants after RevokeAllForUser = %+v", grants)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, tr := e.flow("")
	// A pending request that never completes.
	resp, _ := e.get("/oauth/authorize?" + authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")).Encode())
	location(t, resp)

	if n, err := e.svc.Cleanup(ctx); err != nil || n != 0 {
		t.Errorf("Cleanup now = %d, %v; want 0", n, err)
	}
	e.clock.Advance(2 * time.Hour)
	// request (10m), code (5m), access token (1h)
	if n, err := e.svc.Cleanup(ctx); err != nil || n != 3 {
		t.Errorf("Cleanup after 2h = %d, %v; want 3", n, err)
	}
	if next := e.refresh(clientID, tr.RefreshToken, ""); next.status != http.StatusOK {
		t.Errorf("refresh token removed by cleanup: %d %s", next.status, next.Error)
	}
	e.clock.Advance(31 * 24 * time.Hour)
	// two refresh tokens and the second access token
	if n, err := e.svc.Cleanup(ctx); err != nil || n != 3 {
		t.Errorf("Cleanup after 31 days = %d, %v; want 3", n, err)
	}
}
