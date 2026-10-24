package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Client ID metadata document fetch limits.
const (
	cimdTimeout  = 10 * time.Second
	cimdMaxBytes = 64 << 10
	cimdMaxAge   = time.Hour
)

// Registration limits.
const (
	maxRedirectURIs  = 20
	maxClientNameLen = 200
)

// errInvalidClient means the client id is unknown or its metadata document
// could not be used. The message is safe to show.
var errInvalidClient = errors.New("unknown client")

// clientMetadata is the subset of RFC 7591 client metadata Agalma reads,
// both from a registration request and from a client ID metadata document.
type clientMetadata struct {
	ClientID                string   `json:"client_id,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
}

// validate checks the metadata of a public client and returns the RFC 7591
// error code and a description on failure.
func (m *clientMetadata) validate() (code, desc string) {
	if m.TokenEndpointAuthMethod != "" && m.TokenEndpointAuthMethod != "none" {
		return "invalid_client_metadata", "only public clients (token_endpoint_auth_method \"none\") are supported"
	}
	for _, g := range m.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return "invalid_client_metadata", fmt.Sprintf("unsupported grant type %q", g)
		}
	}
	for _, r := range m.ResponseTypes {
		if r != "code" {
			return "invalid_client_metadata", fmt.Sprintf("unsupported response type %q", r)
		}
	}
	if len(m.ClientName) > maxClientNameLen {
		return "invalid_client_metadata", "client_name is too long"
	}
	if len(m.RedirectURIs) == 0 {
		return "invalid_redirect_uri", "redirect_uris is required"
	}
	if len(m.RedirectURIs) > maxRedirectURIs {
		return "invalid_redirect_uri", "too many redirect_uris"
	}
	for _, u := range m.RedirectURIs {
		if err := validRedirectURI(u); err != nil {
			return "invalid_redirect_uri", err.Error()
		}
	}
	return "", ""
}

// validRedirectURI accepts https URLs and http loopback URLs, without a
// fragment or user info.
func validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("redirect URI %q is not an absolute URL", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil {
		return fmt.Errorf("redirect URI %q must not have a fragment or user info", raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("redirect URI %q must use https, or http on a loopback address", raw)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// matchRedirectURI reports whether requested equals one of the registered
// URIs. For http loopback URIs the port is ignored (RFC 8252 section 7.3);
// scheme, host, path and query must still match.
func matchRedirectURI(registered []string, requested string) bool {
	if slices.Contains(registered, requested) {
		return true
	}
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "http" || !isLoopback(req.Hostname()) || req.User != nil || req.Fragment != "" {
		return false
	}
	for _, r := range registered {
		reg, err := url.Parse(r)
		if err != nil || reg.Scheme != "http" || !isLoopback(reg.Hostname()) {
			continue
		}
		if reg.Hostname() == req.Hostname() && reg.EscapedPath() == req.EscapedPath() && reg.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}

// isMetadataURL reports whether a client id names a client ID metadata
// document.
func isMetadataURL(id string) bool {
	return strings.HasPrefix(id, "https://")
}

// resolveClient returns a registered client, or fetches (and caches) the
// metadata document of a URL client id. Any failure is errInvalidClient,
// wrapped with the reason.
func (s *Service) resolveClient(ctx context.Context, id string) (*Client, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: client_id is required", errInvalidClient)
	}
	c, err := s.store.GetClient(ctx, id)
	switch {
	case err == nil:
		if c.FetchedAt == nil || s.now().Sub(*c.FetchedAt) < cimdMaxAge {
			return c, nil
		}
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	if !isMetadataURL(id) {
		return nil, fmt.Errorf("%w: %q is not registered", errInvalidClient, id)
	}
	fetched, err := s.fetchClient(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "oauth: client metadata fetch failed", "client_id", id, "err", err)
		return nil, fmt.Errorf("%w: %v", errInvalidClient, err)
	}
	if c != nil {
		fetched.CreatedAt = c.CreatedAt
	}
	if err := s.store.UpsertClient(ctx, fetched); err != nil {
		return nil, err
	}
	return fetched, nil
}

// fetchClient downloads and validates the client ID metadata document at
// id.
func (s *Service) fetchClient(ctx context.Context, id string) (*Client, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("client_id is not a valid https URL")
	}
	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// Redirects are not followed: the document must live at its own URL.
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch metadata document: status %s", resp.Status)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "application/json" && !strings.HasSuffix(mt, "+json") {
		return nil, fmt.Errorf("metadata document has content type %q, want application/json", mt)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read metadata document: %w", err)
	}
	if len(body) > cimdMaxBytes {
		return nil, fmt.Errorf("metadata document is larger than %d bytes", cimdMaxBytes)
	}
	var m clientMetadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode metadata document: %w", err)
	}
	if m.ClientID != id {
		return nil, fmt.Errorf("metadata document client_id %q does not equal its URL", m.ClientID)
	}
	if code, desc := m.validate(); code != "" {
		return nil, fmt.Errorf("metadata document: %s", desc)
	}
	now := s.now()
	return &Client{
		ID:           id,
		Name:         strings.TrimSpace(m.ClientName),
		RedirectURIs: m.RedirectURIs,
		CreatedAt:    now,
		FetchedAt:    &now,
	}, nil
}
