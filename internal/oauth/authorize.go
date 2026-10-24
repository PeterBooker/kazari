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

	"agalma/internal/auth"
	"agalma/internal/user"
)

// Names of the cookie that holds the id of the pending authorization
// request. Browsers accept a __Host- cookie only when it is Secure, has no
// Domain and has path /, so a page on a sibling host (an artifact at
// <slug>.<base>, which runs member-authored script) cannot plant one for
// mcp.<base> with Domain=<base>. Over plain http (Options.InsecureCookies)
// the prefix is unusable and the plain name is used.
const (
	CookieName         = "__Host-agalma_oauth"
	InsecureCookieName = "agalma_oauth"
)

// pkceValue matches an S256 challenge or a code verifier (RFC 7636): 43 to
// 128 characters of [A-Za-z0-9-._~].
var pkceValue = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// serveAuthorize validates an authorization request, stores it and sends
// the browser to the identity provider, or to the API-key consent page when
// none is configured.
func (s *Service) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	// Client and redirect URI problems are shown here and never redirected:
	// the redirect target is not trustworthy yet.
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

// parseScopes splits a scope parameter and orders it like Options.Scopes.
// Empty means every supported scope; an unknown scope fails.
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

// checkResource accepts an absent resource or Options.Resource with or
// without a trailing slash, and returns the canonical value to store.
func (s *Service) checkResource(param string) (string, bool) {
	if param == "" {
		return "", true
	}
	if strings.TrimSuffix(param, "/") == strings.TrimSuffix(s.opts.Resource, "/") {
		return s.opts.Resource, true
	}
	return "", false
}

// redirectError sends the browser back to the client with an RFC 6749
// section 4.1.2.1 error.
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

// withQuery appends v to the query of raw.
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

func (s *Service) cookieName() string {
	if s.opts.InsecureCookies {
		return InsecureCookieName
	}
	return CookieName
}

func (s *Service) setCookie(w http.ResponseWriter, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if ttl < 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    value,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !s.opts.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter) { s.setCookie(w, "", -1) }

// pendingRequest loads the live request named by the cookie. ok is false
// after a page has been written.
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

// serveCallback finishes the upstream sign-in, resolves the person to a
// user and shows the consent page.
func (s *Service) serveCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.idp == nil {
		s.message(w, http.StatusNotFound, "Not found", "No identity provider is configured.", "")
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
			"Your account is not allowed to use this Agalma instance. Ask your administrator for access.", id.Email)
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

// sameOrigin reports whether a browser form post comes from a page of this
// server. Same-site is not enough: artifacts on sibling hosts run
// member-authored script. Browsers send Origin on a POST, but "null" from a
// page with Referrer-Policy no-referrer (the consent page itself), so then
// Sec-Fetch-Site decides. A request with neither header comes from a
// non-browser client or a browser too old to send them, where the __Host-
// cookie still holds.
func (s *Service) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		return strings.EqualFold(o, s.origin)
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

// serveConsent handles Approve and Deny on the consent page.
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
	// The request id in the form must equal the cookie: another page can
	// neither read the cookie nor, with the __Host- prefix, set it.
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

// approval is who approved a request and what its code carries.
type approval struct {
	handle    string
	scopes    []string
	via       string // "oidc" or "api_key"
	keyPrefix string // the approving API key, "" via oidc
}

// approver decides who approves the request and which scopes the code
// carries: the user bound at the callback (capped by role), or the owner of
// the API key in the form (capped at the key's scopes), whose grant then
// lives only as long as the key. ok is false after a page has been written.
func (s *Service) approver(w http.ResponseWriter, r *http.Request, req *Request) (approval, bool) {
	ctx := r.Context()
	if req.UserHandle != "" {
		u, err := s.users.GetUser(ctx, req.UserHandle)
		if err == nil && u.DisabledAt != nil {
			err = user.ErrDisabled
		}
		if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrDisabled) {
			s.message(w, http.StatusForbidden, "No access",
				"Your account is not allowed to use this Agalma instance. Ask your administrator for access.", "")
			return approval{}, false
		}
		if err != nil {
			s.log.ErrorContext(ctx, "oauth: load user", "err", err)
			s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
			return approval{}, false
		}
		return approval{handle: u.Handle, scopes: intersect(req.Scopes, user.ScopesFor(u)), via: "oidc"}, true
	}

	// API-key fallback: only when no identity provider is configured.
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
