package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/auth"
)

var (
	alicePrincipal = auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead}, Source: auth.SourceAPIKey, KeyID: "alicekey"}
	bossPrincipal  = auth.Principal{Handle: "boss", Admin: true, Scopes: []string{auth.ScopeAdmin}, Source: auth.SourceAPIKey, KeyID: "bosskey1"}
)

var tokens = auth.AuthenticatorFunc(func(_ context.Context, token string) (auth.Principal, error) {
	switch token {
	case "alice-token":
		return alicePrincipal, nil
	case "narrow-boss-token":
		// An admin with member scopes only. A real authenticator leaves Admin
		// false here; the guard must not rely on it.
		return auth.Principal{Handle: "boss", Admin: true, Scopes: []string{auth.ScopeRead}, Source: auth.SourceAPIKey, KeyID: "bossnarw"}, nil
	case "boss-token":
		return bossPrincipal, nil
	case "broken":
		return auth.Principal{}, errors.New("database down")
	}
	return auth.Principal{}, auth.ErrInvalidToken
})

var echo = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "no principal", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(p)
})

type result struct {
	status    int
	challenge string
	code      string
	principal *auth.Principal
}

func serve(t *testing.T, h http.Handler, authz string, ctx context.Context) result {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := result{status: rec.Code, challenge: rec.Header().Get("WWW-Authenticate")}
	if rec.Code == http.StatusOK {
		var p auth.Principal
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode principal: %v (%s)", err, rec.Body)
		}
		res.principal = &p
		return res
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body)
	}
	if body.Error.Message == "" {
		t.Errorf("error body has no message: %s", rec.Body)
	}
	res.code = body.Error.Code
	return res
}

const plainChallenge = `Bearer error="invalid_token"`

func TestRequireBearer(t *testing.T) {
	const metaURL = "https://mcp.example.test/.well-known/oauth-protected-resource"
	metaChallenge := `Bearer error="invalid_token", resource_metadata="` + metaURL + `"`
	tests := []struct {
		name  string
		opts  auth.Options
		authz string
		want  result
	}{
		{"missing header", auth.Options{}, "",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"missing header with resource metadata", auth.Options{ResourceMetadataURL: metaURL}, "",
			result{status: 401, challenge: metaChallenge, code: "unauthorized"}},
		{"other scheme", auth.Options{}, "Basic YWxpY2U6cGF6eg==",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"empty bearer", auth.Options{}, "Bearer    ",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"invalid token", auth.Options{}, "Bearer nope",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"invalid token with resource metadata", auth.Options{ResourceMetadataURL: metaURL}, "Bearer nope",
			result{status: 401, challenge: metaChallenge, code: "unauthorized"}},
		{"authenticator error", auth.Options{}, "Bearer broken",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"valid token", auth.Options{}, "Bearer alice-token",
			result{status: 200, principal: &alicePrincipal}},
		{"scheme is case-insensitive", auth.Options{ResourceMetadataURL: metaURL}, "bearer  alice-token ",
			result{status: 200, principal: &alicePrincipal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(t, auth.RequireBearer(tokens, tt.opts)(echo), tt.authz, nil)
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(result{})); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Bearer abc", "abc", true},
		{"BEARER abc ", "abc", true},
		{"Token abc", "", false},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		got, ok := auth.BearerToken(req)
		if got != tt.want || ok != tt.ok {
			t.Errorf("BearerToken(%q) = %q, %v; want %q, %v", tt.header, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRequireScope(t *testing.T) {
	h := auth.RequireScope(auth.ScopeWrite)(echo)
	writer := auth.Principal{Handle: "w", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}}
	admin := auth.Principal{Handle: "boss", Admin: true}
	tests := []struct {
		name string
		ctx  context.Context
		want result
	}{
		{"no principal", context.Background(),
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"missing scope", auth.ContextWithPrincipal(context.Background(), alicePrincipal),
			result{status: 403, code: "forbidden"}},
		{"has scope", auth.ContextWithPrincipal(context.Background(), writer),
			result{status: 200, principal: &writer}},
		{"admin passes", auth.ContextWithPrincipal(context.Background(), admin),
			result{status: 200, principal: &admin}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(t, h, "", tt.ctx)
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(result{})); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}

	chained := auth.RequireBearer(tokens, auth.Options{})(auth.RequireScope(auth.ScopeRead)(echo))
	if got := serve(t, chained, "Bearer alice-token", nil); got.status != 200 {
		t.Errorf("bearer + scope: status %d", got.status)
	}
}

func TestRequireAdmin(t *testing.T) {
	adminToken := auth.Principal{Handle: "admin", Admin: true, Scopes: auth.AllScopes, Source: auth.SourceAdminToken}
	tests := []struct {
		name     string
		token    string
		fallback auth.Authenticator
		authz    string
		want     result
	}{
		{"missing header", "s3cret", tokens, "",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"config token", "s3cret", nil, "Bearer s3cret",
			result{status: 200, principal: &adminToken}},
		{"config token with fallback", "s3cret", tokens, "Bearer s3cret",
			result{status: 200, principal: &adminToken}},
		{"wrong token", "s3cret", nil, "Bearer s3cret2",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"prefix of token", "s3cret", nil, "Bearer s3cre",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"wrong token, fallback rejects", "s3cret", tokens, "Bearer nope",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"empty config token disables token path", "", nil, "Bearer s3cret",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
		{"fallback admin passes", "s3cret", tokens, "Bearer boss-token",
			result{status: 200, principal: &bossPrincipal}},
		{"fallback admin passes with token path disabled", "", tokens, "Bearer boss-token",
			result{status: 200, principal: &bossPrincipal}},
		{"fallback non-admin forbidden", "s3cret", tokens, "Bearer alice-token",
			result{status: 403, code: "forbidden"}},
		{"fallback admin role without admin scope forbidden", "s3cret", tokens, "Bearer narrow-boss-token",
			result{status: 403, code: "forbidden"}},
		{"fallback error", "s3cret", tokens, "Bearer broken",
			result{status: 401, challenge: plainChallenge, code: "unauthorized"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(t, auth.RequireAdmin(tt.token, tt.fallback, auth.Options{})(echo), tt.authz, nil)
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(result{})); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}
