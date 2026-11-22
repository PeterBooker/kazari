package oauth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/PeterBooker/kazari/internal/instrument"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/user"
)

// tokenResponse is the RFC 6749 section 5.1 success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Service) serveToken(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.token")
	defer func() {
		grant := grantTypeKey.String(instrument.OneOf(r.PostForm.Get("grant_type"), "authorization_code", "refresh_token"))
		s.m.tokens.Add(r.Context(), 1, out.attrs(grant))
		if span.IsRecording() {
			span.SetAttributes(grant)
		}
		endHandle(span, out)
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if clientID == "" {
		writeOAuthError(w, r, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return
	}
	if _, err := s.store.GetClient(r.Context(), clientID); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeOAuthError(w, r, http.StatusUnauthorized, "invalid_client", "unknown client")
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
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		writeOAuthError(w, r, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Service) grantCode(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	f := r.PostForm
	code, verifier, redirectURI := f.Get("code"), f.Get("code_verifier"), f.Get("redirect_uri")
	switch {
	case code == "":
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "code is required")
		return
	case verifier == "":
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "code_verifier is required")
		return
	case redirectURI == "":
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		return
	case !pkceValue.MatchString(verifier):
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "code_verifier is malformed")
		return
	}
	resource, ok := s.checkResource(f.Get("resource"))
	if !ok {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}

	now := s.now()
	// Consume first: a code is spent by any attempt, failed or not.
	c, err := s.store.ConsumeCode(ctx, hashSecret(code), now)
	if errors.Is(err, ErrCodeUsed) {
		// One of the two presenters holds a stolen code, and nothing says
		// which, so the tokens the first one got are revoked (OAuth 2.1
		// section 4.1.2).
		s.log.WarnContext(ctx, "oauth: authorization code reused", "client_id", clientID, "family", c.FamilyID)
		if c.FamilyID != 0 {
			if err := s.store.RevokeFamily(ctx, c.FamilyID, now); err != nil {
				s.serverError(w, r, "revoke family", err)
				return
			}
		}
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid or has been used")
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid or has been used")
		return
	}
	if err != nil {
		s.serverError(w, r, "consume code", err)
		return
	}
	switch {
	case c.ClientID != clientID:
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the authorization code was issued to another client")
		return
	case !now.Before(c.ExpiresAt):
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the authorization code has expired")
		return
	case c.RedirectURI != redirectURI:
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case subtle.ConstantTimeCompare([]byte(s256(verifier)), []byte(c.CodeChallenge)) != 1:
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code challenge")
		return
	case resource != "" && c.Resource != "" && resource != c.Resource:
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
		return
	}
	if !s.userActive(w, r, c.UserHandle) {
		return
	}
	keyExpires, ok := s.keyActive(w, r, c.KeyPrefix, c.UserHandle)
	if !ok {
		return
	}
	if resource == "" {
		resource = c.Resource
	}
	access, refresh := s.newPair(Token{
		ClientID: clientID, UserHandle: c.UserHandle, Scopes: c.Scopes, Resource: resource, KeyPrefix: c.KeyPrefix,
	}, keyExpires, now)
	if err := s.store.CreateTokens(ctx, &access.Token, &refresh.Token); err != nil {
		s.serverError(w, r, "store tokens", err)
		return
	}
	if err := s.store.LinkCodeFamily(ctx, c.Hash, refresh.FamilyID); err != nil {
		// The tokens are issued; a reuse of this code then revokes nothing.
		s.log.ErrorContext(ctx, "oauth: link code to its tokens", "err", err)
	}
	s.writeTokens(w, access, refresh)
}

func (s *Service) grantRefresh(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	f := r.PostForm
	raw := f.Get("refresh_token")
	if raw == "" {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	old, err := s.store.GetToken(ctx, hashSecret(raw))
	if errors.Is(err, ErrNotFound) {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	if err != nil {
		s.serverError(w, r, "load refresh token", err)
		return
	}
	now := s.now()
	if old.Kind != KindRefresh || old.ClientID != clientID {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	rotated := old.ReplacedBy != nil
	// A rotated token came back. Inside the reuse window it is most likely
	// a client whose answer was lost, so the family may get one more pair;
	// otherwise someone holds a stolen copy, and the whole family is
	// revoked (OAuth 2.1 section 4.3.1).
	reusable := s.opts.RefreshReuseWindow > 0 && now.Before(old.ExpiresAt) &&
		(!rotated || old.RevokedAt != nil && now.Sub(*old.RevokedAt) < s.opts.RefreshReuseWindow)
	if rotated && !reusable {
		s.replayed(w, r, old, now)
		return
	}
	if !rotated && (old.RevokedAt != nil || !now.Before(old.ExpiresAt)) {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the refresh token has expired or been revoked")
		return
	}
	scopes := old.Scopes
	if sc := strings.Fields(f.Get("scope")); len(sc) > 0 {
		for _, x := range sc {
			if !slices.Contains(old.Scopes, x) || !slices.Contains(s.opts.Scopes, x) {
				writeOAuthError(w, r, http.StatusBadRequest, "invalid_scope", "scope may only narrow the original grant")
				return
			}
		}
		scopes = intersect(old.Scopes, sc)
	}
	if !s.userActive(w, r, old.UserHandle) {
		return
	}
	keyExpires, ok := s.keyActive(w, r, old.KeyPrefix, old.UserHandle)
	if !ok {
		return
	}
	access, refresh := s.newPair(Token{
		ClientID: clientID, UserHandle: old.UserHandle, Scopes: scopes, Resource: old.Resource, KeyPrefix: old.KeyPrefix,
	}, keyExpires, now)

	err = ErrNotFound
	if !rotated {
		// ErrNotFound here is a race lost to a concurrent refresh of the
		// same token, which the reuse window covers like a lost answer.
		err = s.store.Rotate(ctx, old.ID, now, &access.Token, &refresh.Token)
	}
	if errors.Is(err, ErrNotFound) && reusable {
		err = s.store.Reissue(ctx, old.ID, now.Add(-s.opts.RefreshReuseWindow), &access.Token, &refresh.Token)
	}
	switch {
	case err == nil:
		s.writeTokens(w, access, refresh)
	case !errors.Is(err, ErrNotFound):
		s.serverError(w, r, "rotate refresh token", err)
	case rotated:
		s.replayed(w, r, old, now)
	default:
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the refresh token has already been used")
	}
}

func (s *Service) replayed(w http.ResponseWriter, r *http.Request, old *Token, now time.Time) {
	ctx := r.Context()
	s.log.WarnContext(ctx, "oauth: refresh token replay, revoking family",
		"client_id", old.ClientID, "user", old.UserHandle, "family", old.FamilyID)
	if err := s.store.RevokeFamily(ctx, old.FamilyID, now); err != nil {
		s.serverError(w, r, "revoke family", err)
		return
	}
	s.record(ctx, old.UserHandle, ActionRevoke, old.UserHandle, map[string]string{
		"grant": fmt.Sprint(old.FamilyID), "client_id": old.ClientID, "reason": ReasonRefreshReplay,
	})
	writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the refresh token has already been used")
}

// userActive writes invalid_grant when the user is gone or disabled.
func (s *Service) userActive(w http.ResponseWriter, r *http.Request, handle string) bool {
	u, err := s.users.GetUser(r.Context(), handle)
	if errors.Is(err, user.ErrNotFound) || (err == nil && u.DisabledAt != nil) {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the user may no longer sign in")
		return false
	}
	if err != nil {
		s.serverError(w, r, "load user", err)
		return false
	}
	return true
}

// keyActive writes invalid_grant when the approving API key is no longer
// active, and returns its expiry: nil when it has none or there is no key.
func (s *Service) keyActive(w http.ResponseWriter, r *http.Request, prefix, handle string) (*time.Time, bool) {
	key, err := s.approvingKey(r.Context(), prefix, handle)
	if err != nil {
		s.serverError(w, r, "check api key", err)
		return nil, false
	}
	if !key.live {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the API key that approved this grant is no longer valid")
		return nil, false
	}
	return key.expires, true
}

type issued struct {
	Token
	secret string
}

// newPair caps scopes at the offered ones, and each token expires after its
// lifetime or at notAfter (the approving key's expiry), whichever is first.
func (s *Service) newPair(grant Token, notAfter *time.Time, now time.Time) (access, refresh issued) {
	scopes := intersect(grant.Scopes, s.opts.Scopes)
	mk := func(kind string, ttl time.Duration) issued {
		secret := randomString(secretBytes)
		expires := now.Add(ttl)
		if notAfter != nil && notAfter.Before(expires) {
			expires = *notAfter
		}
		return issued{
			secret: secret,
			Hash:   hashSecret(secret), Kind: kind, ClientID: grant.ClientID, UserHandle: grant.UserHandle,
			Scopes: scopes, Resource: grant.Resource, KeyPrefix: grant.KeyPrefix, CreatedAt: now, ExpiresAt: expires,
		}
	}
	return mk(KindAccess, s.opts.AccessTTL), mk(KindRefresh, s.opts.RefreshTTL)
}

func (s *Service) writeTokens(w http.ResponseWriter, access, refresh issued) {
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access.secret,
		TokenType:    "Bearer",
		ExpiresIn:    int64(access.ExpiresAt.Sub(access.CreatedAt) / time.Second),
		RefreshToken: refresh.secret,
		Scope:        strings.Join(access.Scopes, " "),
	})
}

func (s *Service) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), "oauth: "+what, "err", err)
	writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "internal error")
}
