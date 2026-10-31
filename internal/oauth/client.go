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

	"github.com/PeterBooker/kazari/internal/fetch"
)

const (
	cimdTimeout  = 10 * time.Second
	cimdMaxBytes = 64 << 10
	cimdMaxAge   = time.Hour
)

const (
	maxRedirectURIs  = 20
	maxClientNameLen = 200
)

// errInvalidClient means an unknown client id or an unusable metadata
// document; its message is safe to show.
var errInvalidClient = errors.New("unknown client")

// clientMetadata is the subset of RFC 7591 client metadata read from
// registrations and client ID metadata documents.
type clientMetadata struct {
	ClientID                string   `json:"client_id,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
}

// validate returns an RFC 7591 error code and description, or "" if valid.
// Grant and response types Kazari does not offer are ignored, not refused: a
// client lists what it uses with any server (claude.ai lists the JWT bearer
// grant), and the authorize and token endpoints accept only the code flow.
func (m *clientMetadata) validate() (code, desc string) {
	if m.TokenEndpointAuthMethod != "" && m.TokenEndpointAuthMethod != "none" {
		return "invalid_client_metadata", "only public clients (token_endpoint_auth_method \"none\") are supported"
	}
	if len(m.GrantTypes) > 0 && !slices.Contains(m.GrantTypes, "authorization_code") {
		return "invalid_client_metadata", "grant_types must include authorization_code"
	}
	if len(m.ResponseTypes) > 0 && !slices.Contains(m.ResponseTypes, "code") {
		return "invalid_client_metadata", "response_types must include code"
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

// matchRedirectURI matches exactly, except that the port of an http loopback
// URI is ignored (RFC 8252 section 7.3).
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

func isMetadataURL(id string) bool {
	return strings.HasPrefix(id, "https://")
}

// resolveClient fetches and caches the metadata document of a URL client id.
// Errors other than the store's wrap errInvalidClient with the reason.
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
	if c == nil {
		out := &outcome{}
		if err != nil {
			out.code = "invalid_client_metadata"
		}
		s.m.registrations.Add(ctx, 1, out.attrs(registrationMethodKey.String("cimd")))
	}
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

func (s *Service) fetchClient(ctx context.Context, id string) (*Client, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("client_id is not a valid https URL")
	}
	ctx, cancel := context.WithTimeout(fetch.WithPurpose(ctx, fetch.PurposeCIMD), cimdTimeout)
	defer cancel()
	start := time.Now()
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
		s.opts.Fetch.Done(ctx, start, 0, fetch.ErrorType(err))
		return nil, fmt.Errorf("fetch metadata document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.opts.Fetch.Done(ctx, start, 0, fetch.StatusErrorType(resp.StatusCode))
		return nil, fmt.Errorf("fetch metadata document: status %s", resp.Status)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "application/json" && !strings.HasSuffix(mt, "+json") {
		return nil, fmt.Errorf("metadata document has content type %q, want application/json", mt)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		s.opts.Fetch.Done(ctx, start, 0, fetch.ErrorType(err))
		return nil, fmt.Errorf("read metadata document: %w", err)
	}
	if len(body) > cimdMaxBytes {
		s.opts.Fetch.Done(ctx, start, 0, fetch.ErrTooLarge)
		return nil, fmt.Errorf("metadata document is larger than %d bytes", cimdMaxBytes)
	}
	s.opts.Fetch.Done(ctx, start, int64(len(body)), "")
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
