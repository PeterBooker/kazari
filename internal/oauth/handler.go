package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/PeterBooker/kazari/internal/middleware"
)

// Endpoint paths, relative to the issuer.
const (
	PathAuthServerMeta = "/.well-known/oauth-authorization-server"
	PathResourceMeta   = "/.well-known/oauth-protected-resource"
	PathRegister       = "/oauth/register"
	PathAuthorize      = "/oauth/authorize"
	PathCallback       = "/oauth/callback"
	PathConsent        = "/oauth/consent"
	PathToken          = "/oauth/token"
)

const maxBodyBytes = 64 << 10

// Handler serves the metadata documents and /oauth/, browser sign-in for
// gated artifacts included. Mount it at the root of the MCP host beside the
// MCP endpoint; it also claims paths below PathResourceMeta.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+PathAuthServerMeta, cors(http.HandlerFunc(s.serveAuthServerMeta)))
	mux.Handle("GET "+PathResourceMeta, cors(http.HandlerFunc(s.serveResourceMeta)))
	mux.Handle("GET "+PathResourceMeta+"/", cors(http.HandlerFunc(s.serveResourceMeta)))
	mux.Handle("POST "+PathRegister, cors(http.HandlerFunc(s.serveRegister)))
	mux.Handle("POST "+PathToken, cors(http.HandlerFunc(s.serveToken)))
	for _, p := range []string{PathAuthServerMeta, PathResourceMeta, PathResourceMeta + "/", PathRegister, PathToken} {
		mux.Handle("OPTIONS "+p, cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))
	}
	mux.HandleFunc("GET "+PathAuthorize, s.serveAuthorize)
	mux.HandleFunc("GET "+PathCallback, s.serveCallback)
	mux.HandleFunc("POST "+PathConsent, s.serveConsent)
	mux.HandleFunc("GET "+PathSignIn, s.serveSignIn)
	mux.HandleFunc("POST "+PathSignIn, s.serveSignInKey)
	return middleware.Routed(mux)
}

// cors lets browser-based MCP clients call the JSON endpoints; none use
// cookies, so any origin is fine.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		h.Set("Access-Control-Max-Age", "86400")
		next.ServeHTTP(w, r)
	})
}

// authServerMeta is RFC 8414 authorization server metadata.
type authServerMeta struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

func (s *Service) serveAuthServerMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, authServerMeta{
		Issuer:                            s.opts.Issuer,
		AuthorizationEndpoint:             s.opts.Issuer + PathAuthorize,
		TokenEndpoint:                     s.opts.Issuer + PathToken,
		RegistrationEndpoint:              s.opts.Issuer + PathRegister,
		ScopesSupported:                   s.opts.Scopes,
		ResponseTypesSupported:            []string{"code"},
		ResponseModesSupported:            []string{"query"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		ClientIDMetadataDocumentSupported: true,
	})
}

// resourceMeta is RFC 9728 protected resource metadata.
type resourceMeta struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

func (s *Service) serveResourceMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, resourceMeta{
		Resource:               s.opts.Resource,
		AuthorizationServers:   []string{s.opts.Issuer},
		ScopesSupported:        s.opts.Scopes,
		BearerMethodsSupported: []string{"header"},
	})
}

// registrationResponse is the RFC 7591 client information response.
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func (s *Service) serveRegister(w http.ResponseWriter, r *http.Request) {
	var m clientMetadata
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&m); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body is too large")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body is not valid JSON client metadata")
		return
	}
	if code, desc := m.validate(); code != "" {
		writeOAuthError(w, http.StatusBadRequest, code, desc)
		return
	}
	now := s.now()
	c := &Client{
		ID:           "dcr_" + randomString(18),
		Name:         strings.TrimSpace(m.ClientName),
		RedirectURIs: slices.Compact(m.RedirectURIs),
		CreatedAt:    now,
	}
	if err := s.store.UpsertClient(r.Context(), c); err != nil {
		s.log.ErrorContext(r.Context(), "oauth: register client", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not store the client")
		return
	}
	s.log.InfoContext(r.Context(), "oauth: client registered", "client_id", c.ID, "client_name", c.Name)
	writeJSON(w, http.StatusCreated, registrationResponse{
		ClientID:                c.ID,
		ClientIDIssuedAt:        now.Unix(),
		ClientName:              c.Name,
		RedirectURIs:            c.RedirectURIs,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError is the RFC 6749 section 5.2 / RFC 7591 section 3.2.2 body.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer error="`+code+`"`)
	}
	writeJSON(w, status, oauthError{Error: code, ErrorDescription: desc})
}
