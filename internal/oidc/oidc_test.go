package oidc_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/auth"
	"kazari/internal/oidc"
)

const (
	clientID = "kazari-test"
	verifier = "the-pkce-verifier-the-pkce-verifier-0123456789"
	nonce    = "nonce-123"
)

// fakeOP is an OpenID provider: discovery, JWKS and a token endpoint that
// returns an ID token signed with a test RSA key.
type fakeOP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu              sync.Mutex
	discoveryIssuer string         // "" = the server URL
	claims          map[string]any // ID token claims
	signer          *rsa.PrivateKey
}

func newFakeOP(t *testing.T) *fakeOP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &fakeOP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		op.mu.Lock()
		iss := op.discoveryIssuer
		op.mu.Unlock()
		if iss == "" {
			iss = op.srv.URL
		}
		writeJSON(w, map[string]any{
			"issuer":                                iss,
			"authorization_endpoint":                op.srv.URL + "/authorize",
			"token_endpoint":                        op.srv.URL + "/token",
			"jwks_uri":                              op.srv.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := op.key.PublicKey
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()),
			"e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not used", http.StatusNotImplemented)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, _, _ := r.BasicAuth()
		if r.PostForm.Get("client_id") != clientID && user != clientID {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "invalid_client"})
			return
		}
		if r.PostForm.Get("code") != "good-code" || r.PostForm.Get("code_verifier") != verifier {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		op.mu.Lock()
		claims, signer := op.claims, op.signer
		op.mu.Unlock()
		if signer == nil {
			signer = op.key
		}
		writeJSON(w, map[string]any{
			"access_token": "upstream-access", "token_type": "Bearer", "expires_in": 3600,
			"id_token": signJWT(t, signer, claims),
		})
	})
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	return op
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWT builds an RS256 JWT by hand.
func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Error(err)
	}
	input := b64(header) + "." + b64(payload)
	h := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Error(err)
	}
	return input + "." + b64(sig)
}

// baseClaims are valid claims for the fake provider.
func (op *fakeOP) baseClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":            op.srv.URL,
		"sub":            "user-1",
		"aud":            clientID,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"nonce":          nonce,
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	}
}

func (op *fakeOP) provider(t *testing.T) *oidc.Provider {
	t.Helper()
	p, err := oidc.New(context.Background(), oidc.Config{
		Issuer:      op.srv.URL,
		ClientID:    clientID,
		RedirectURL: "https://mcp.art.example/oauth/callback",
		HTTPClient:  op.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExchange(t *testing.T) {
	op := newFakeOP(t)
	p := op.provider(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		edit     func(c map[string]any)
		signer   *rsa.PrivateKey
		code     string
		verifier string
		nonce    string
		want     auth.Identity
		wantErr  string
	}{
		{name: "good", want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}},
		{name: "audience list", edit: func(c map[string]any) { c["aud"] = []string{"other", clientID} },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}},
		{name: "email_verified string true", edit: func(c map[string]any) { c["email_verified"] = "true" },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}},
		{name: "email_verified string false", edit: func(c map[string]any) { c["email_verified"] = "false" },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", Name: "Alice"}},
		{name: "email_verified false", edit: func(c map[string]any) { c["email_verified"] = false },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", Name: "Alice"}},
		{name: "email_verified absent", edit: func(c map[string]any) { delete(c, "email_verified") },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", Name: "Alice"}},
		// The fallback is Entra's only (see TestIdentityMapping).
		{name: "preferred_username ignored outside entra", edit: func(c map[string]any) {
			delete(c, "email")
			c["preferred_username"] = "alice@corp.example"
		}, want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Name: "Alice"}},
		{name: "preferred_username without at", edit: func(c map[string]any) {
			delete(c, "email")
			c["preferred_username"] = "alice"
		}, want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Name: "Alice"}},
		{name: "email wins over preferred_username", edit: func(c map[string]any) { c["preferred_username"] = "other@corp.example" },
			want: auth.Identity{Issuer: op.srv.URL, Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}},

		{name: "wrong nonce", nonce: "other-nonce", wantErr: "nonce"},
		{name: "missing nonce", edit: func(c map[string]any) { delete(c, "nonce") }, wantErr: "nonce"},
		{name: "wrong audience", edit: func(c map[string]any) { c["aud"] = "someone-else" }, wantErr: "audience"},
		{name: "wrong issuer", edit: func(c map[string]any) { c["iss"] = "https://evil.example" }, wantErr: "issued by a different provider"},
		{name: "expired", edit: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, wantErr: "expired"},
		{name: "foreign signature", signer: other, wantErr: "verify"},
		{name: "wrong code", code: "bad-code", wantErr: "exchange"},
		{name: "wrong verifier", verifier: "another-verifier-another-verifier-0123456789", wantErr: "exchange"},
		{name: "empty code", code: "-", wantErr: "no code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := op.baseClaims()
			if tt.edit != nil {
				tt.edit(claims)
			}
			op.mu.Lock()
			op.claims, op.signer = claims, tt.signer
			op.mu.Unlock()

			code, v, n := "good-code", verifier, nonce
			if tt.code == "-" {
				code = ""
			} else if tt.code != "" {
				code = tt.code
			}
			if tt.verifier != "" {
				v = tt.verifier
			}
			if tt.nonce != "" {
				n = tt.nonce
			}
			got, err := p.Exchange(context.Background(), code, n, v)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAuthURL(t *testing.T) {
	op := newFakeOP(t)
	p := op.provider(t)
	if p.Issuer() != op.srv.URL {
		t.Errorf("Issuer() = %q", p.Issuer())
	}
	u, err := url.Parse(p.AuthURL("st", "nn", "challenge"))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != op.srv.URL+"/authorize" {
		t.Errorf("endpoint = %s", got)
	}
	want := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"https://mcp.art.example/oauth/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {"st"},
		"nonce":                 {"nn"},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
	}
	if diff := cmp.Diff(want, u.Query()); diff != "" {
		t.Errorf("query (-want +got):\n%s", diff)
	}
}

func TestNew(t *testing.T) {
	op := newFakeOP(t)
	base := oidc.Config{Issuer: op.srv.URL, ClientID: clientID, RedirectURL: "https://x/cb", HTTPClient: op.srv.Client()}
	tests := []struct {
		name            string
		cfg             func(c *oidc.Config)
		discoveryIssuer string
		wantErr         bool
	}{
		{"ok", func(*oidc.Config) {}, "", false},
		{"missing issuer", func(c *oidc.Config) { c.Issuer = "" }, "", true},
		{"missing client id", func(c *oidc.Config) { c.ClientID = "" }, "", true},
		{"missing redirect", func(c *oidc.Config) { c.RedirectURL = "" }, "", true},
		{"unreachable", func(c *oidc.Config) { c.Issuer = op.srv.URL + "/nothing-here" }, "", true},
		{"issuer mismatch", func(*oidc.Config) {}, "https://elsewhere.example", true},
		{"issuer mismatch allowed for tests", func(c *oidc.Config) { c.InsecureSkipIssuerCheck = true }, "https://elsewhere.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op.mu.Lock()
			op.discoveryIssuer = tt.discoveryIssuer
			op.mu.Unlock()
			cfg := base
			tt.cfg(&cfg)
			_, err := oidc.New(context.Background(), cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("New err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
