package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
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
			"scopes_supported":                      []any{"artifacts:read", "artifacts:write", "artifacts:delete"},
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
			"scopes_supported":         []any{"artifacts:read", "artifacts:write", "artifacts:delete"},
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
		{"other grants beside authorization_code", `{"redirect_uris":["https://app.example/cb"],"grant_types":["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:jwt-bearer"]}`, 201, ""},
		{"implicit grant", `{"redirect_uris":["https://app.example/cb"],"grant_types":["implicit"]}`, 400, "invalid_client_metadata"},
		{"other response types beside code", `{"redirect_uris":["https://app.example/cb"],"response_types":["code","token"]}`, 201, ""},
		{"no code response type", `{"redirect_uris":["https://app.example/cb"],"response_types":["token"]}`, 400, "invalid_client_metadata"},
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
		{"admin scope", with("scope", "artifacts:read admin"), "invalid_scope"},
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

// An artifact on a sibling host must not overwrite the cookie via
// Domain=<base> and so defeat the consent form check: browsers refuse a
// __Host- cookie with a Domain, without Secure or off path /.
func TestRequestCookie(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		e := newEnv(t, envOptions{})
		srv, name := e.svc, oauth.InsecureCookieName
		if !insecure {
			srv = oauth.New(e.store, e.users, nil, e.idp, nil, oauth.Options{Issuer: e.srv.URL, Resource: e.srv.URL, Now: e.clock.Now})
			name = oauth.CookieName
		}
		clientID := e.register("https://app.example/cb")
		rec := &responseRecorder{header: http.Header{}}
		req, _ := http.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeParams(clientID, "https://app.example/cb", "", newPKCE("v")).Encode(), nil)
		srv.Handler().ServeHTTP(rec, req)
		ck := rec.header.Get("Set-Cookie")
		if !strings.HasPrefix(ck, name+"=") {
			t.Errorf("insecure=%v: Set-Cookie %q, want name %s", insecure, ck, name)
		}
		for _, want := range []string{"; Path=/;", "Max-Age=600", "HttpOnly", "SameSite=Lax"} {
			if !strings.Contains(ck, want) {
				t.Errorf("Set-Cookie %q lacks %q", ck, want)
			}
		}
		if strings.Contains(ck, "Domain=") {
			t.Errorf("Set-Cookie %q has a Domain", ck)
		}
		if got := strings.Contains(ck, "Secure"); got == insecure {
			t.Errorf("insecure=%v: Set-Cookie %q", insecure, ck)
		}
	}
	if oauth.CookieName != "__Host-"+oauth.InsecureCookieName {
		t.Errorf("CookieName = %q, want the __Host- prefix", oauth.CookieName)
	}
}

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
		scope      string
		wantScopes []string
	}{
		{"member gets member scopes", auth.Identity{Issuer: "https://idp.example", Subject: "a", Email: "alice@example.com", EmailVerified: true},
			"alice", "", auth.MemberScopes},
		{"admin gets member scopes", auth.Identity{Issuer: "https://idp.example", Subject: "b", Email: "boss@example.com", EmailVerified: true},
			"boss", "", auth.MemberScopes},
		{"narrow request", auth.Identity{Issuer: "https://idp.example", Subject: "b", Email: "boss@example.com", EmailVerified: true},
			"boss", "artifacts:read", []string{auth.ScopeRead}},
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
			want := auth.Principal{Handle: tt.wantHandle, Scopes: tt.wantScopes, Source: auth.SourceOAuth, KeyID: p2.KeyID}
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
	for _, tok := range []string{"", "kzk_abcdefgh_x", strings.Repeat("a", 43)} {
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
				req.AddCookie(&http.Cookie{Name: oauth.InsecureCookieName, Value: real})
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

	resp, body := e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}, "api_key": {"kzk_wrong"}})
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
	location(t, resp)
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
	unused := e.register("https://idle.example/cb")
	clientID, tr := e.flow("")
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
	if _, err := e.svc.GetClient(ctx, unused); err != nil {
		t.Errorf("client without a grant removed after 2h: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)
	// two refresh tokens, the second access token and the client that
	// never completed a grant (7 days)
	if n, err := e.svc.Cleanup(ctx); err != nil || n != 4 {
		t.Errorf("Cleanup after 31 days = %d, %v; want 4", n, err)
	}
	if _, err := e.svc.GetClient(ctx, unused); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("GetClient(unused) err = %v, want ErrNotFound", err)
	}
	if _, err := e.svc.GetClient(ctx, clientID); err != nil {
		t.Errorf("client with a grant removed: %v", err)
	}
}

// adminHostStatus guards the request as internal/app does when no admin
// token is configured.
func adminHostStatus(a auth.Authenticator, token string) int {
	h := auth.RequireAdmin("", a)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// Registration is open, so a client an admin approves must not get a token
// the admin host accepts.
func TestAdminScopeNeverGranted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	e.idp.setIdentity(auth.Identity{Issuer: "https://idp.example", Subject: "b", Email: "boss@example.com", EmailVerified: true})
	clientID, tr := e.flow("")
	if tr.Scope != strings.Join(auth.MemberScopes, " ") {
		t.Errorf("granted scope = %q, want the member scopes", tr.Scope)
	}
	if got := adminHostStatus(e.svc, tr.AccessToken); got != http.StatusForbidden {
		t.Errorf("admin host with an admin's OAuth token: status %d, want 403", got)
	}

	// Tokens stored with admin before the rule are capped too, and so are
	// their refreshes.
	accessSecret, refreshSecret := strings.Repeat("a", 43), strings.Repeat("b", 43)
	now := e.clock.Now()
	stored := func(secret, kind string) *oauth.Token {
		h := sha256.Sum256([]byte(secret))
		return &oauth.Token{Hash: hex.EncodeToString(h[:]), Kind: kind, ClientID: clientID, UserHandle: "boss",
			Scopes: auth.AllScopes, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	if err := e.store.CreateTokens(ctx, stored(accessSecret, oauth.KindAccess), stored(refreshSecret, oauth.KindRefresh)); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.Authenticate(ctx, accessSecret)
	if err != nil {
		t.Fatal(err)
	}
	want := auth.Principal{Handle: "boss", Scopes: auth.MemberScopes, Source: auth.SourceOAuth, KeyID: p.KeyID}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("principal of a stored admin-scoped token (-want +got):\n%s", diff)
	}
	if got := adminHostStatus(e.svc, accessSecret); got != http.StatusForbidden {
		t.Errorf("admin host with a stored admin-scoped token: status %d, want 403", got)
	}
	if r := e.refresh(clientID, refreshSecret, "admin"); r.Error != "invalid_scope" {
		t.Errorf("refresh asking for admin = %d %q, want invalid_scope", r.status, r.Error)
	}
	if r := e.refresh(clientID, refreshSecret, ""); r.status != http.StatusOK || r.Scope != strings.Join(auth.MemberScopes, " ") {
		t.Errorf("refresh of a stored admin-scoped grant = %d %q scope %q, want the member scopes", r.status, r.Error, r.Scope)
	}

	svc := oauth.New(e.store, e.users, nil, nil, nil, oauth.Options{Issuer: e.srv.URL, Resource: e.srv.URL + "/", Scopes: auth.AllScopes})
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var meta struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(auth.MemberScopes, meta.ScopesSupported); diff != "" {
		t.Errorf("scopes_supported with Options.Scopes = AllScopes (-want +got):\n%s", diff)
	}
}

type failingKeys struct {
	oauth.Keys
	err error
}

func (k failingKeys) KeyStatus(context.Context, string) (user.KeyStatus, error) {
	return user.KeyStatus{}, k.err
}

// A grant approved with an API key must end when the key is revoked or
// expires, however often it is refreshed.
func TestGrantEndsWithApprovingKey(t *testing.T) {
	ctx := context.Background()
	const redirect = "http://localhost/callback"
	p := newPKCE("apikey")
	// approved approves with a new key of carol's that expires after
	// expiresIn (0 = never).
	approved := func(t *testing.T, expiresIn time.Duration) (*env, *user.APIKey, string, string) {
		t.Helper()
		e := newEnv(t, envOptions{noIDP: true})
		if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
			t.Fatal(err)
		}
		k, key, err := e.users.IssueKey(ctx, "carol", "laptop", nil, expiresIn)
		if err != nil {
			t.Fatal(err)
		}
		clientID := e.register(redirect)
		e.signIn(authorizeParams(clientID, redirect, "", p))
		return e, k, clientID, e.approve(url.Values{"api_key": {key}})
	}
	exchange := func(t *testing.T, e *env, clientID, code string) tokenResult {
		t.Helper()
		tr := e.exchange(clientID, redirect, code, p)
		if tr.status != http.StatusOK {
			t.Fatalf("exchange = %d %s: %s", tr.status, tr.Error, tr.ErrorDescription)
		}
		return tr
	}

	t.Run("revoked key", func(t *testing.T) {
		e, k, clientID, code := approved(t, 0)
		tr := exchange(t, e, clientID, code)
		if g := e.rec.actions(oauth.ActionGrant); len(g) != 1 || g[0].Details["key_prefix"] != k.Prefix {
			t.Errorf("grant audit = %+v, want key_prefix %s", g, k.Prefix)
		}
		if _, err := e.svc.Authenticate(ctx, tr.AccessToken); err != nil {
			t.Fatal(err)
		}
		if err := e.users.RevokeKey(ctx, "carol", k.Prefix); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("access token after the key was revoked: err = %v, want ErrInvalidToken", err)
		}
		if r := e.refresh(clientID, tr.RefreshToken, ""); r.status != http.StatusBadRequest || r.Error != "invalid_grant" {
			t.Errorf("refresh after the key was revoked = %d %q, want invalid_grant", r.status, r.Error)
		}
	})
	t.Run("key revoked before the exchange", func(t *testing.T) {
		e, k, clientID, code := approved(t, 0)
		if err := e.users.RevokeKey(ctx, "carol", k.Prefix); err != nil {
			t.Fatal(err)
		}
		if tr := e.exchange(clientID, redirect, code, p); tr.status != http.StatusBadRequest || tr.Error != "invalid_grant" {
			t.Errorf("exchange = %d %q, want invalid_grant", tr.status, tr.Error)
		}
	})
	t.Run("expiring key caps the grant", func(t *testing.T) {
		e, k, clientID, code := approved(t, 90*time.Minute)
		tr := exchange(t, e, clientID, code)
		if tr.ExpiresIn != 3600 {
			t.Errorf("expires_in = %d, want 3600", tr.ExpiresIn)
		}
		grants, err := e.svc.ListGrants(ctx, "carol")
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 1 || !grants[0].ExpiresAt.Equal(*k.ExpiresAt) || grants[0].KeyPrefix != k.Prefix {
			t.Errorf("grants = %+v, want one of key %s expiring with it at %v", grants, k.Prefix, k.ExpiresAt)
		}
		e.clock.Advance(time.Hour)
		next := e.refresh(clientID, tr.RefreshToken, "")
		if next.status != http.StatusOK || next.ExpiresIn != 1800 {
			t.Fatalf("refresh 30 minutes before the key expires = %d %q, expires_in %d; want 1800", next.status, next.Error, next.ExpiresIn)
		}
		e.clock.Advance(30 * time.Minute)
		if _, err := e.svc.Authenticate(ctx, next.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("access token once the key expired: err = %v, want ErrInvalidToken", err)
		}
		if r := e.refresh(clientID, next.RefreshToken, ""); r.Error != "invalid_grant" {
			t.Errorf("refresh once the key expired = %d %q, want invalid_grant", r.status, r.Error)
		}
	})
	t.Run("no key checker", func(t *testing.T) {
		e, _, clientID, code := approved(t, 0)
		tr := exchange(t, e, clientID, code)
		svc := oauth.New(e.store, e.users, nil, nil, nil, oauth.Options{Issuer: e.srv.URL, Now: e.clock.Now})
		if _, err := svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("Authenticate without Keys: err = %v, want ErrInvalidToken", err)
		}
	})
	t.Run("key check fails", func(t *testing.T) {
		e, _, clientID, code := approved(t, 0)
		tr := exchange(t, e, clientID, code)
		boom := errors.New("disk on fire")
		svc := oauth.New(e.store, e.users, failingKeys{e.users, boom}, nil, nil, oauth.Options{Issuer: e.srv.URL, Now: e.clock.Now})
		if _, err := svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, boom) || errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("Authenticate = %v, want the storage error", err)
		}
	})
}

// A KeyID that changed on refresh would let a client reset its
// per-credential write rate limit and split its audit trail.
func TestKeyIDStableAcrossRefresh(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, first := e.flow("")
	p1, err := e.svc.Authenticate(ctx, first.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	second := e.refresh(clientID, first.RefreshToken, "")
	if second.status != http.StatusOK {
		t.Fatalf("refresh = %d %s", second.status, second.Error)
	}
	p2, err := e.svc.Authenticate(ctx, second.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if p1.KeyID != p2.KeyID || !strings.HasPrefix(p1.KeyID, "oauth:") {
		t.Errorf("KeyID before and after refresh = %q, %q; want the same oauth:<grant>", p1.KeyID, p2.KeyID)
	}
	_, other := e.flow("")
	p3, err := e.svc.Authenticate(ctx, other.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if p3.KeyID == p1.KeyID {
		t.Errorf("two grants share KeyID %q", p1.KeyID)
	}
}

// Re-enabling a user must not bring back a grant an attacker held.
func TestDisableRevokesGrants(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	clientID, tr := e.flow("")
	for _, disabled := range []bool{true, false} {
		if _, err := e.users.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("access token after disable and enable: err = %v, want ErrInvalidToken", err)
	}
	if r := e.refresh(clientID, tr.RefreshToken, ""); r.Error != "invalid_grant" {
		t.Errorf("refresh after disable and enable = %d %q, want invalid_grant", r.status, r.Error)
	}
	if grants, err := e.svc.ListGrants(ctx, "alice"); err != nil || len(grants) != 0 {
		t.Errorf("ListGrants after disable = %+v, %v; want none", grants, err)
	}
	if ev := e.rec.actions(oauth.ActionRevoke); len(ev) != 1 || ev[0].Details["user"] != "alice" {
		t.Errorf("revoke audit = %+v", ev)
	}
	_, fresh := e.flow("")
	if _, err := e.svc.Authenticate(ctx, fresh.AccessToken); err != nil {
		t.Errorf("new grant after enable: %v", err)
	}
}

const mcpOrigin = "https://mcp.art.example.com"

// mcpHost is the server as deployed (https, Secure cookies); carol approves
// with her API key.
type mcpHost struct {
	t        *testing.T
	svc      *oauth.Service
	clientID string
	key      string
}

func newMCPHost(t *testing.T) *mcpHost {
	t.Helper()
	e := newEnv(t, envOptions{noIDP: true})
	ctx := context.Background()
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.users.IssueKey(ctx, "carol", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &mcpHost{t: t, key: key, clientID: e.register("https://app.example/cb"),
		svc: oauth.New(e.store, e.users, e.users, nil, nil, oauth.Options{Issuer: mcpOrigin, Resource: mcpOrigin + "/", Now: e.clock.Now})}
}

func (h *mcpHost) serve(req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	h.svc.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func (h *mcpHost) authorize(p pkce) string {
	h.t.Helper()
	q := authorizeParams(h.clientID, "https://app.example/cb", "", p)
	resp := h.serve(httptest.NewRequest(http.MethodGet, mcpOrigin+"/oauth/authorize?"+q.Encode(), nil))
	for _, c := range resp.Cookies() {
		if c.Name == oauth.CookieName {
			return c.Value
		}
	}
	h.t.Fatalf("authorize set no %s cookie: %q", oauth.CookieName, resp.Header["Set-Cookie"])
	return ""
}

// consent posts Approve for request id, sent in a cookie named cookie.
func (h *mcpHost) consent(id, cookie string, header http.Header) *http.Response {
	form := url.Values{"request_id": {id}, "action": {"approve"}, "api_key": {h.key}}
	req := httptest.NewRequest(http.MethodPost, mcpOrigin+"/oauth/consent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.AddCookie(&http.Cookie{Name: cookie, Value: id})
	return h.serve(req)
}

// A sibling host is same-site, so SameSite=Lax lets its posts through: only
// the server's own page may post the consent form. The token and
// registration endpoints serve CORS and non-browser clients and do not care.
func TestConsentRequiresSameOrigin(t *testing.T) {
	h := newMCPHost(t)
	id := h.authorize(newPKCE("v"))
	refused := []struct {
		name   string
		header http.Header
	}{
		{"artifact host", http.Header{"Origin": {"https://evil.art.example.com"}, "Sec-Fetch-Site": {"same-site"}}},
		{"apex", http.Header{"Origin": {"https://art.example.com"}}},
		{"plain http", http.Header{"Origin": {"http://mcp.art.example.com"}}},
		{"other site", http.Header{"Origin": {"https://attacker.example"}, "Sec-Fetch-Site": {"cross-site"}}},
		{"null origin from a sibling host", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-site"}}},
		{"no origin, same-site", http.Header{"Sec-Fetch-Site": {"same-site"}}},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.consent(id, oauth.CookieName, tt.header)
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
				t.Errorf("status = %d, Location %q, want 403", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}

	accepted := []struct {
		name   string
		header http.Header
	}{
		{"same origin", http.Header{"Origin": {mcpOrigin}, "Sec-Fetch-Site": {"same-origin"}}},
		// The consent page's no-referrer policy makes browsers send this.
		{"null origin, same-origin fetch", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-origin"}}},
		{"no browser headers", nil},
	}
	for i, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			p := newPKCE("v")
			if i > 0 { // the first approves the request the refused posts left alone
				id = h.authorize(p)
			}
			resp := h.consent(id, oauth.CookieName, tt.header)
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("status = %d, want 302", resp.StatusCode)
			}
			code := location(t, resp).Query().Get("code")
			if code == "" {
				t.Fatalf("redirect %s has no code", resp.Header.Get("Location"))
			}
			form := url.Values{"grant_type": {"authorization_code"}, "client_id": {h.clientID},
				"redirect_uri": {"https://app.example/cb"}, "code": {code}, "code_verifier": {p.verifier}}
			req := httptest.NewRequest(http.MethodPost, mcpOrigin+"/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "https://evil.art.example.com")
			if resp := h.serve(req); resp.StatusCode != http.StatusOK {
				t.Errorf("cross-origin token request: status %d, want 200", resp.StatusCode)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, mcpOrigin+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://app.example/cb"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://claude.ai")
	if resp := h.serve(req); resp.StatusCode != http.StatusCreated {
		t.Errorf("cross-origin registration: status %d, want 201", resp.StatusCode)
	}
}

// A sibling-host script could set a Domain=<base> cookie to a request it
// started, sign the visitor in for it and post the form. Over https only the
// __Host- name is read, which browsers never accept with a Domain.
func TestConsentIgnoresTossedCookie(t *testing.T) {
	h := newMCPHost(t)
	id := h.authorize(newPKCE("v"))
	if resp := h.consent(id, oauth.InsecureCookieName, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("consent with the cookie under its plain name: status %d, want 403", resp.StatusCode)
	}
	resp := h.consent(id, oauth.CookieName, nil)
	if resp.StatusCode != http.StatusFound || location(t, resp).Query().Get("code") == "" {
		t.Errorf("consent with the __Host- cookie: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
