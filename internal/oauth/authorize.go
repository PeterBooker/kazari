package oauth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/user"
)

// Names of the pending request's cookie. Browsers accept a __Host- cookie
// only when it is Secure, on path / and without Domain, so script in an
// artifact on a sibling host cannot plant one with Domain=<base>. Plain http
// (Options.InsecureCookies) cannot use the prefix.
const (
	CookieName         = "__Host-kazari_oauth"
	InsecureCookieName = "kazari_oauth"
)

// pkceValue matches an S256 challenge or a code verifier (RFC 7636).
var pkceValue = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func (s *Service) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	// Client and redirect URI errors are shown, never redirected: the target
	// is not trusted yet.
	c, err := s.resolveClient(ctx, q.Get("client_id"))
	if err != nil {
		if errors.Is(err, errInvalidClient) {
			s.message(w, http.StatusBadRequest, "Unknown application",
				"The application that sent you here is not registered with this server.", err.Error())
			return
		}
		s.log.ErrorContext(ctx, "oauth: authorize", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	if redirectURI == "" || !matchRedirectURI(c.RedirectURIs, redirectURI) {
		s.message(w, http.StatusBadRequest, "Invalid redirect address",
			"The application asked to return you to an address it has not registered.", redirectURI)
		return
	}

	state := q.Get("state")
	fail := func(code, desc string) { redirectError(w, r, redirectURI, state, code, desc) }

	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		fail("invalid_request", "code_challenge is required (PKCE)")
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "code_challenge_method must be S256")
		return
	}
	if len(challenge) != 43 || !pkceValue.MatchString(challenge) {
		fail("invalid_request", "code_challenge is not an S256 challenge")
		return
	}
	scopes, ok := s.parseScopes(q.Get("scope"))
	if !ok {
		fail("invalid_scope", "unknown scope requested")
		return
	}
	resource, ok := s.checkResource(q.Get("resource"))
	if !ok {
		fail("invalid_target", "unknown resource")
		return
	}

	req := &Request{
		ID:            randomString(secretBytes),
		ClientID:      c.ID,
		RedirectURI:   redirectURI,
		Scopes:        scopes,
		CodeChallenge: challenge,
		Resource:      resource,
		ClientState:   state,
		ExpiresAt:     s.now().Add(s.opts.RequestTTL),
	}
	var verifier string
	if s.idp != nil {
		verifier = oauth2.GenerateVerifier()
		req.UpstreamState = randomString(secretBytes)
		req.Nonce = randomString(secretBytes)
		req.UpstreamVerifier = verifier
	}
	if err := s.store.CreateRequest(ctx, req); err != nil {
		s.log.ErrorContext(ctx, "oauth: store request", "err", err)
		fail("server_error", "could not store the request")
		return
	}
	s.setCookie(w, req.ID, s.opts.RequestTTL)

	if s.idp != nil {
		http.Redirect(w, r, s.idp.AuthURL(req.UpstreamState, req.Nonce, oauth2.S256ChallengeFromVerifier(verifier)), http.StatusFound)
		return
	}
	s.renderConsent(w, http.StatusOK, c, req, "", scopes, "")
}

// parseScopes orders scopes like Options.Scopes; empty means all of them and
// an unknown one fails.
func (s *Service) parseScopes(param string) ([]string, bool) {
	fields := strings.Fields(param)
	if len(fields) == 0 {
		return slices.Clone(s.opts.Scopes), true
	}
	for _, f := range fields {
		if !slices.Contains(s.opts.Scopes, f) {
			return nil, false
		}
	}
	return intersect(s.opts.Scopes, fields), true
}

// checkResource accepts "" or Options.Resource with or without a trailing
// slash, and returns the value to store.
func (s *Service) checkResource(param string) (string, bool) {
	if param == "" {
		return "", true
	}
	if strings.TrimSuffix(param, "/") == strings.TrimSuffix(s.opts.Resource, "/") {
		return s.opts.Resource, true
	}
	return "", false
}

// redirectError sends an RFC 6749 section 4.1.2.1 error to the client.
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	v := url.Values{"error": {code}}
	if desc != "" {
		v.Set("error_description", desc)
	}
	if state != "" {
		v.Set("state", state)
	}
	http.Redirect(w, r, withQuery(redirectURI, v), http.StatusFound)
}

func withQuery(raw string, v url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range v {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Service) cookieName() string { return s.named(InsecureCookieName) }

// named returns a cookie name with the __Host- prefix, or base itself over
// plain http (Options.InsecureCookies).
func (s *Service) named(base string) string {
	if s.opts.InsecureCookies {
		return base
	}
	return "__Host-" + base
}

func (s *Service) setCookie(w http.ResponseWriter, value string, ttl time.Duration) {
	s.setNamedCookie(w, s.cookieName(), value, ttl)
}

// setNamedCookie sets a host-only cookie; a negative ttl deletes it.
func (s *Service) setNamedCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if ttl < 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !s.opts.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter) { s.setCookie(w, "", -1) }

// pendingRequest loads the live request named by the cookie; when ok is
// false a page has been written.
func (s *Service) pendingRequest(w http.ResponseWriter, r *http.Request) (*Request, bool) {
	ck, err := r.Cookie(s.cookieName())
	if err != nil || ck.Value == "" {
		s.message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Start again from your application.", "")
		return nil, false
	}
	req, err := s.store.GetRequest(r.Context(), ck.Value)
	if err == nil && !s.now().Before(req.ExpiresAt) {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		s.clearCookie(w)
		s.message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Start again from your application.", "")
		return nil, false
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "oauth: load request", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return nil, false
	}
	return req, true
}

func (s *Service) serveCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.idp == nil {
		s.message(w, http.StatusNotFound, "Not found", "No identity provider is configured.", "")
		return
	}
	// The identity provider has one redirect URI for both flows; the state
	// says which one this is.
	if strings.HasPrefix(r.URL.Query().Get("state"), signInStatePrefix) {
		s.serveSignInCallback(w, r)
		return
	}
	req, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(req.UpstreamState)) != 1 {
		s.message(w, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response does not belong to this browser session. Start again from your application.", "")
		return
	}
	if e := q.Get("error"); e != "" {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.clearCookie(w)
		desc := "sign-in at the identity provider failed: " + e
		if d := q.Get("error_description"); d != "" {
			desc += ": " + d
		}
		redirectError(w, r, req.RedirectURI, req.ClientState, "access_denied", desc)
		return
	}
	if req.UserHandle != "" {
		s.message(w, http.StatusBadRequest, "Sign-in already used",
			"This sign-in has already been completed. Start again from your application.", "")
		return
	}

	id, err := s.idp.Exchange(ctx, q.Get("code"), req.Nonce, req.UpstreamVerifier)
	if err != nil {
		s.log.WarnContext(ctx, "oauth: identity provider exchange", "err", err)
		s.message(w, http.StatusBadGateway, "Sign-in failed",
			"The identity provider's response could not be verified. Start again from your application.", "")
		return
	}
	u, err := s.users.ResolveIdentity(ctx, id)
	if errors.Is(err, user.ErrNotProvisioned) || errors.Is(err, user.ErrDisabled) {
		s.log.InfoContext(ctx, "oauth: identity not admitted", "issuer", id.Issuer, "subject", id.Subject, "email", id.Email, "err", err)
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.clearCookie(w)
		s.message(w, http.StatusForbidden, "No access",
			"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", id.Email)
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: resolve identity", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if err := s.store.BindRequest(ctx, req.ID, u.Handle); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.message(w, http.StatusBadRequest, "Sign-in already used",
				"This sign-in has already been completed. Start again from your application.", "")
			return
		}
		s.log.ErrorContext(ctx, "oauth: bind request", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	req.UserHandle = u.Handle

	scopes := intersect(req.Scopes, user.ScopesFor(u))
	if len(scopes) == 0 {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.clearCookie(w)
		redirectError(w, r, req.RedirectURI, req.ClientState, "invalid_scope", "none of the requested scopes is available to this account")
		return
	}
	c, err := s.store.GetClient(ctx, req.ClientID)
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: load client", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.renderConsent(w, http.StatusOK, c, req, u.Handle, scopes, "")
}

func (s *Service) renderConsent(w http.ResponseWriter, status int, c *Client, req *Request, handle string, scopes []string, errMsg string) {
	name := c.Name
	if name == "" {
		name = c.ID
	}
	host := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	s.pages.render(w, status, "consent", consentPage{
		ClientName:   name,
		ClientID:     c.ID,
		RedirectHost: host,
		Handle:       handle,
		Scopes:       scopeViews(scopes),
		RequestID:    req.ID,
		AskKey:       handle == "",
		Error:        errMsg,
	}, req.RedirectURI)
}

// sameOrigin reports whether a form post comes from this server's pages.
// Same-site is not enough: artifacts on sibling hosts run member-authored
// script. The consent page's no-referrer policy makes Origin "null", so then
// Sec-Fetch-Site decides; with neither header (a non-browser client or an
// old browser) the __Host- cookie still holds.
func (s *Service) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		return strings.EqualFold(o, s.origin)
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func (s *Service) serveConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.sameOrigin(r) {
		s.message(w, http.StatusForbidden, "Request rejected",
			"This form was not sent from this server's sign-in page. Start again from your application.", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.message(w, http.StatusBadRequest, "Bad request", "The form could not be read.", "")
		return
	}
	// The form's request id must equal the cookie, which another page can
	// neither read nor, with the __Host- prefix, set.
	ck, err := r.Cookie(s.cookieName())
	formID := r.PostForm.Get("request_id")
	if err != nil || formID == "" || subtle.ConstantTimeCompare([]byte(formID), []byte(ck.Value)) != 1 {
		s.message(w, http.StatusForbidden, "Request rejected",
			"This form does not belong to the current sign-in. Start again from your application.", "")
		return
	}
	req, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}

	switch r.PostForm.Get("action") {
	case "deny":
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.clearCookie(w)
		redirectError(w, r, req.RedirectURI, req.ClientState, "access_denied", "the user denied the request")
		return
	case "approve":
	default:
		s.message(w, http.StatusBadRequest, "Bad request", "Choose Approve or Deny.", "")
		return
	}

	a, ok := s.approver(w, r, req)
	if !ok {
		return
	}
	if len(a.scopes) == 0 {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.clearCookie(w)
		redirectError(w, r, req.RedirectURI, req.ClientState, "invalid_scope", "none of the requested scopes is available to this account")
		return
	}

	code := randomString(secretBytes)
	err = s.store.CreateCode(ctx, &Code{
		Hash:          hashSecret(code),
		ClientID:      req.ClientID,
		UserHandle:    a.handle,
		Scopes:        a.scopes,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Resource:      req.Resource,
		KeyPrefix:     a.keyPrefix,
		ExpiresAt:     s.now().Add(s.opts.CodeTTL),
	})
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: store code", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if err := s.store.DeleteRequest(ctx, req.ID); err != nil {
		s.log.WarnContext(ctx, "oauth: delete request", "err", err)
	}
	s.clearCookie(w)
	details := map[string]string{"client_id": req.ClientID, "scopes": strings.Join(a.scopes, " "), "via": a.via}
	if a.keyPrefix != "" {
		details["key_prefix"] = a.keyPrefix
	}
	s.record(ctx, a.handle, ActionGrant, details)
	v := url.Values{"code": {code}}
	if req.ClientState != "" {
		v.Set("state", req.ClientState)
	}
	http.Redirect(w, r, withQuery(req.RedirectURI, v), http.StatusFound)
}

type approval struct {
	handle    string
	scopes    []string
	via       string // "oidc" or "api_key"
	keyPrefix string // the approving API key, "" via oidc
}

// approver is the user bound at the callback (scopes capped by role) or the
// owner of the form's API key (capped at the key's scopes), whose grant then
// lives only as long as the key. When ok is false a page has been written.
func (s *Service) approver(w http.ResponseWriter, r *http.Request, req *Request) (approval, bool) {
	ctx := r.Context()
	if req.UserHandle != "" {
		u, err := s.users.GetUser(ctx, req.UserHandle)
		if err == nil && u.DisabledAt != nil {
			err = user.ErrDisabled
		}
		if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrDisabled) {
			s.message(w, http.StatusForbidden, "No access",
				"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", "")
			return approval{}, false
		}
		if err != nil {
			s.log.ErrorContext(ctx, "oauth: load user", "err", err)
			s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
			return approval{}, false
		}
		return approval{handle: u.Handle, scopes: intersect(req.Scopes, user.ScopesFor(u)), via: "oidc"}, true
	}

	if s.idp != nil || s.keys == nil {
		s.message(w, http.StatusForbidden, "Sign-in required",
			"Sign in at the identity provider first. Start again from your application.", "")
		return approval{}, false
	}
	p, err := s.keys.Authenticate(ctx, strings.TrimSpace(r.PostForm.Get("api_key")))
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidToken) {
			s.log.ErrorContext(ctx, "oauth: authenticate api key", "err", err)
		}
		c, cerr := s.store.GetClient(ctx, req.ClientID)
		if cerr != nil {
			s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
			return approval{}, false
		}
		s.renderConsent(w, http.StatusUnauthorized, c, req, "", req.Scopes, "That API key was not accepted.")
		return approval{}, false
	}
	return approval{handle: p.Handle, scopes: intersect(req.Scopes, p.Scopes), via: "api_key", keyPrefix: p.KeyID}, true
}
