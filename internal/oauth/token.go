package oauth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"agalma/internal/user"
)

// tokenResponse is the RFC 6749 section 5.1 success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// serveToken is the token endpoint: authorization_code and refresh_token
// grants for public clients.
func (s *Service) serveToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if clientID == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return
	}
	if _, err := s.store.GetClient(r.Context(), clientID); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
			return
		}
		s.serverError(w, r, "load client", err)
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		s.grantCode(w, r, clientID)
	case "refresh_token":
		s.grantRefresh(w, r, clientID)
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Service) grantCode(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	f := r.PostForm
	code, verifier, redirectURI := f.Get("code"), f.Get("code_verifier"), f.Get("redirect_uri")
	switch {
	case code == "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	case verifier == "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code_verifier is required")
		return
	case redirectURI == "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		return
	case !pkceValue.MatchString(verifier):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code_verifier is malformed")
		return
	}
	resource, ok := s.checkResource(f.Get("resource"))
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}

	now := s.now()
	// Consume first: a code is spent by any attempt, failed or not.
	c, err := s.store.ConsumeCode(ctx, hashSecret(code), now)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCodeUsed) {
		if errors.Is(err, ErrCodeUsed) {
			s.log.WarnContext(ctx, "oauth: authorization code reused", "client_id", clientID)
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid or has been used")
		return
	}
	if err != nil {
		s.serverError(w, r, "consume code", err)
		return
	}
	switch {
	case c.ClientID != clientID:
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code was issued to another client")
		return
	case !now.Before(c.ExpiresAt):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code has expired")
		return
	case c.RedirectURI != redirectURI:
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case subtle.ConstantTimeCompare([]byte(s256(verifier)), []byte(c.CodeChallenge)) != 1:
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code challenge")
		return
	case resource != "" && c.Resource != "" && resource != c.Resource:
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
		return
	}
	if !s.userActive(w, r, c.UserHandle) {
		return
	}
	if resource == "" {
		resource = c.Resource
	}
	access, refresh := s.newPair(clientID, c.UserHandle, c.Scopes, resource, now)
	if err := s.store.CreateTokens(ctx, &access.Token, &refresh.Token); err != nil {
		s.serverError(w, r, "store tokens", err)
		return
	}
	s.writeTokens(w, access, refresh)
}

func (s *Service) grantRefresh(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	f := r.PostForm
	raw := f.Get("refresh_token")
	if raw == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	old, err := s.store.GetToken(ctx, hashSecret(raw))
	if errors.Is(err, ErrNotFound) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	if err != nil {
		s.serverError(w, r, "load refresh token", err)
		return
	}
	now := s.now()
	if old.Kind != KindRefresh || old.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	if old.ReplacedBy != nil {
		// A rotated token came back: either the client or an attacker holds
		// a stolen copy. Kill the whole chain (OAuth 2.1 section 4.3.1).
		s.log.WarnContext(ctx, "oauth: refresh token replay, revoking family",
			"client_id", clientID, "user", old.UserHandle, "family", old.FamilyID)
		if err := s.store.RevokeFamily(ctx, old.FamilyID, now); err != nil {
			s.serverError(w, r, "revoke family", err)
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has already been used")
		return
	}
	if old.RevokedAt != nil || !now.Before(old.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has expired or been revoked")
		return
	}
	scopes := old.Scopes
	if sc := strings.Fields(f.Get("scope")); len(sc) > 0 {
		for _, x := range sc {
			if !slices.Contains(old.Scopes, x) {
				writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "scope may only narrow the original grant")
				return
			}
		}
		scopes = intersect(old.Scopes, sc)
	}
	if !s.userActive(w, r, old.UserHandle) {
		return
	}
	access, refresh := s.newPair(clientID, old.UserHandle, scopes, old.Resource, now)
	err = s.store.Rotate(ctx, old.ID, now, &access.Token, &refresh.Token)
	if errors.Is(err, ErrNotFound) {
		// Lost a race with a concurrent refresh of the same token.
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has already been used")
		return
	}
	if err != nil {
		s.serverError(w, r, "rotate refresh token", err)
		return
	}
	s.writeTokens(w, access, refresh)
}

// userActive writes invalid_grant when the user is gone or disabled.
func (s *Service) userActive(w http.ResponseWriter, r *http.Request, handle string) bool {
	u, err := s.users.GetUser(r.Context(), handle)
	if errors.Is(err, user.ErrNotFound) || (err == nil && u.DisabledAt != nil) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the user may no longer sign in")
		return false
	}
	if err != nil {
		s.serverError(w, r, "load user", err)
		return false
	}
	return true
}

// issued is a token row with its secret, before the secret is thrown away.
type issued struct {
	Token
	secret string
}

func (s *Service) newPair(clientID, handle string, scopes []string, resource string, now time.Time) (access, refresh issued) {
	mk := func(kind string, ttl time.Duration) issued {
		secret := randomString(secretBytes)
		return issued{secret: secret, Token: Token{
			Hash: hashSecret(secret), Kind: kind, ClientID: clientID, UserHandle: handle,
			Scopes: scopes, Resource: resource, CreatedAt: now, ExpiresAt: now.Add(ttl),
		}}
	}
	return mk(KindAccess, s.opts.AccessTTL), mk(KindRefresh, s.opts.RefreshTTL)
}

func (s *Service) writeTokens(w http.ResponseWriter, access, refresh issued) {
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access.secret,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.opts.AccessTTL / time.Second),
		RefreshToken: refresh.secret,
		Scope:        strings.Join(access.Scopes, " "),
	})
}

func (s *Service) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), "oauth: "+what, "err", err)
	writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
}
