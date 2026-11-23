package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/user"
)

// Names of the pending request's cookie, with the __Host- prefix and over
// plain http (Options.InsecureCookies).
const (
	requestCookie      = "kazari_oauth"
	CookieName         = "__Host-" + requestCookie
	InsecureCookieName = requestCookie
)

// maxStateLen bounds the client state stored with a request before sign-in.
const maxStateLen = 1 << 10

// pkceValue matches an S256 challenge or a code verifier (RFC 7636).
var pkceValue = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func (s *Service) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.authorize")
	defer endHandle(span, out)
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
		if errors.Is(err, ErrClientLimit) {
			s.log.WarnContext(ctx, "oauth: client limit reached", "limit", s.opts.MaxUnusedClients)
			s.message(w, http.StatusServiceUnavailable, "Try again later",
				"This server cannot accept new applications right now.", "")
			return
		}
		s.fail(w, r, "authorize", err)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	// The length is checked here too because a loopback match ignores the port
	// and the requested string is what gets stored.
	if redirectURI == "" || len(redirectURI) > maxRedirectURILen || !matchRedirectURI(c.RedirectURIs, redirectURI) {
		s.message(w, http.StatusBadRequest, "Invalid redirect address",
			"The application asked to return you to an address it has not registered.", redirectURI)
		return
	}

	fail := func(code, desc string) { s.invalidRequest(w, code, desc) }

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
	if len(q.Get("state")) > maxStateLen {
		fail("invalid_request", "state is too long")
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
		ClientState:   q.Get("state"),
		ExpiresAt:     s.now().Add(s.opts.RequestTTL),
	}
	if s.idp != nil && s.signIn.Unavailable(w, r) {
		return
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
	s.cookies.Set(w, requestCookie, req.ID, s.opts.RequestTTL)

	if s.idp != nil {
		http.Redirect(w, r, s.idp.AuthURL(req.UpstreamState, req.Nonce, oauth2.S256ChallengeFromVerifier(verifier)), http.StatusFound)
		return
	}
	if !s.signIn.Passwords() {
		s.renderConsent(w, http.StatusOK, c, req, "", scopes, "")
		return
	}
	// A browser that is signed in already goes straight to the consent
	// page; one signed in with an API key signs in again, because a key
	// approves a grant on the consent page, where it bounds the grant.
	sess, err := s.signIn.Session(r)
	if err != nil {
		s.fail(w, r, "load session", err)
		return
	}
	if sess != nil && sess.KeyPrefix == "" {
		s.continueRequest(w, r, req.ID, sess.UserHandle)
		return
	}
	s.signIn.ShowRequestForm(w, r, signin.RequestForm{ID: req.ID, Client: s.clientName(ctx, req), Bind: true})
}

// clientName is the name the sign-in form gives the client of req.
func (s *Service) clientName(ctx context.Context, req *Request) string {
	if c, err := s.store.GetClient(ctx, req.ClientID); err == nil && c.Name != "" {
		return c.Name
	}
	return req.ClientID
}

// requests are the pending requests, as the sign-in form continues them.
type requests struct{ s *Service }

func (q requests) Form(w http.ResponseWriter, r *http.Request, id string) (signin.RequestForm, bool) {
	req, ok := q.s.formRequest(w, r, id)
	if !ok {
		return signin.RequestForm{}, false
	}
	return signin.RequestForm{ID: req.ID, Client: q.s.clientName(r.Context(), req)}, true
}

func (q requests) Continue(w http.ResponseWriter, r *http.Request, id, handle string) {
	q.s.continueRequest(w, r, id, handle)
}

// formRequest loads the pending request a form names. The form's request
// id must equal the cookie, which another page can neither read nor, with
// the __Host- prefix, set. When ok is false a page has been written.
func (s *Service) formRequest(w http.ResponseWriter, r *http.Request, id string) (*Request, bool) {
	if id == "" || subtle.ConstantTimeCompare([]byte(id), []byte(s.cookies.Get(r, requestCookie))) != 1 {
		s.message(w, http.StatusForbidden, "Request rejected",
			"This form does not belong to the current sign-in. Start again from your application.", "")
		return nil, false
	}
	return s.pendingRequest(w, r)
}

// continueRequest binds a pending request to the person who just signed in
// and sends the browser to the consent page.
func (s *Service) continueRequest(w http.ResponseWriter, r *http.Request, id, handle string) {
	if !s.bind(w, r, id, handle) {
		return
	}
	http.Redirect(w, r, PathConsent, http.StatusSeeOther)
}

// bind binds a pending request to the person who signed in for it. When ok
// is false a page has been written.
func (s *Service) bind(w http.ResponseWriter, r *http.Request, id, handle string) bool {
	err := s.store.BindRequest(r.Context(), id, handle)
	if errors.Is(err, ErrNotFound) {
		s.message(w, http.StatusBadRequest, "Sign-in already used",
			"This sign-in has already been completed. Start again from your application.", "")
		return false
	}
	if err != nil {
		s.fail(w, r, "bind request", err)
		return false
	}
	return true
}

// serveConsentPage shows the consent page of the pending request to the
// person who signed in for it with a password. Without a sign-in yet it
// asks for one, or with ?with=key shows the page that takes an API key in
// place of a sign-in.
func (s *Service) serveConsentPage(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.consent")
	defer endHandle(span, out)
	ctx := r.Context()
	req, ok := s.pendingRequest(w, r)
	if !ok {
		return
	}
	c, err := s.store.GetClient(ctx, req.ClientID)
	if err != nil {
		s.fail(w, r, "load client", err)
		return
	}
	if req.UserHandle == "" {
		switch {
		case s.idp != nil:
			s.message(w, http.StatusForbidden, "Sign-in required",
				"Sign in at the identity provider first. Start again from your application.", "")
		case !s.signIn.Passwords() || (s.keys != nil && r.URL.Query().Get(signin.ParamWith) == signin.WithKey):
			s.renderConsent(w, http.StatusOK, c, req, "", req.Scopes, "")
		default:
			s.signIn.ShowRequestForm(w, r, signin.RequestForm{ID: req.ID, Client: s.clientName(ctx, req)})
		}
		return
	}
	u, ok := s.signedIn(w, r, req.UserHandle)
	if !ok {
		return
	}
	scopes := intersect(req.Scopes, user.ScopesFor(u))
	if len(scopes) == 0 {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.cookies.Delete(w, requestCookie)
		s.invalidRequest(w, "invalid_scope", "none of the requested scopes is available to this account")
		return
	}
	s.renderConsent(w, http.StatusOK, c, req, u.Handle, scopes, "")
}

// signedIn returns the person who signed in for a request, if they may
// still use this instance. When ok is false a page has been written.
func (s *Service) signedIn(w http.ResponseWriter, r *http.Request, handle string) (*user.User, bool) {
	u, err := s.users.ActiveUser(r.Context(), handle)
	if errors.Is(err, user.ErrNotFound) {
		s.message(w, http.StatusForbidden, "No access",
			"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", "")
		return nil, false
	}
	if err != nil {
		s.fail(w, r, "load user", err)
		return nil, false
	}
	return u, true
}

// invalidRequest answers a request the server cannot act on with a page.
// Anyone can register a client, so until the person has approved or denied
// the request on the consent page, which names the redirect host, the
// redirect URI is not trusted and nothing is sent there: a redirect would
// make this host an open redirector (RFC 9700 section 4.11.2).
func (s *Service) invalidRequest(w http.ResponseWriter, code, desc string) {
	s.message(w, http.StatusBadRequest, "Invalid request",
		"The application sent a request this server cannot accept. Start again from your application.",
		code+": "+desc)
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

// redirectError sends an RFC 6749 section 4.1.2.1 error to the client. Only
// a decision made on the consent page may end here; see invalidRequest.
func (s *Service) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	v := url.Values{"error": {code}, "iss": {s.opts.Issuer}}
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
	maps.Copy(q, v)
	u.RawQuery = q.Encode()
	return u.String()
}

// pendingRequest loads the live request named by the cookie; when ok is
// false a page has been written.
func (s *Service) pendingRequest(w http.ResponseWriter, r *http.Request) (*Request, bool) {
	id := s.cookies.Get(r, requestCookie)
	if id == "" {
		s.message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Start again from your application.", "")
		return nil, false
	}
	req, err := s.store.GetRequest(r.Context(), id)
	if err == nil && !s.now().Before(req.ExpiresAt) {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		s.cookies.Delete(w, requestCookie)
		s.message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Start again from your application.", "")
		return nil, false
	}
	if err != nil {
		s.fail(w, r, "load request", err)
		return nil, false
	}
	return req, true
}

func (s *Service) serveCallback(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.callback")
	defer endHandle(span, out)
	ctx := r.Context()
	if s.idp == nil {
		s.message(w, http.StatusNotFound, "Not found", "No identity provider is configured.", "")
		return
	}
	// The identity provider has one redirect URI for both flows; the state
	// says which one this is.
	if strings.HasPrefix(r.URL.Query().Get("state"), signin.StatePrefix) {
		s.signIn.ServeCallback(w, r)
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
		s.cookies.Delete(w, requestCookie)
		if d := q.Get("error_description"); d != "" {
			e += ": " + d
		}
		s.message(w, http.StatusBadRequest, "Sign-in failed",
			"The identity provider did not complete the sign-in. Start again from your application.", e)
		return
	}
	if req.UserHandle != "" {
		s.message(w, http.StatusBadRequest, "Sign-in already used",
			"This sign-in has already been completed. Start again from your application.", "")
		return
	}

	u, ok := s.signIn.Identify(w, r, req.Nonce, req.UpstreamVerifier, "Start again from your application.", func() {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.cookies.Delete(w, requestCookie)
	})
	if !ok {
		return
	}
	if !s.bind(w, r, req.ID, u.Handle) {
		return
	}
	req.UserHandle = u.Handle

	scopes := intersect(req.Scopes, user.ScopesFor(u))
	if len(scopes) == 0 {
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.cookies.Delete(w, requestCookie)
		s.invalidRequest(w, "invalid_scope", "none of the requested scopes is available to this account")
		return
	}
	c, err := s.store.GetClient(ctx, req.ClientID)
	if err != nil {
		s.fail(w, r, "load client", err)
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
	s.pages.Render(w, status, "consent", consentPage{
		ClientName:   name,
		ClientID:     c.ID,
		RedirectHost: host,
		Handle:       handle,
		Scopes:       scopeViews(scopes),
		RequestID:    req.ID,
		AskKey:       handle == "",
		OfferKey:     handle != "" && s.keys != nil,
		Error:        errMsg,
	}, req.RedirectURI)
}

// sameOrigin reports whether a form post comes from this server's pages.
func (s *Service) sameOrigin(r *http.Request) bool {
	return middleware.SameOrigin(r, s.origin)
}

func (s *Service) serveConsent(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.consent")
	defer endHandle(span, out)
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
	req, ok := s.formRequest(w, r, r.PostForm.Get("request_id"))
	if !ok {
		return
	}

	switch r.PostForm.Get("action") {
	case "deny":
		_ = s.store.DeleteRequest(ctx, req.ID)
		s.cookies.Delete(w, requestCookie)
		s.redirectError(w, r, req.RedirectURI, req.ClientState, "access_denied", "the user denied the request")
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
		s.cookies.Delete(w, requestCookie)
		s.redirectError(w, r, req.RedirectURI, req.ClientState, "invalid_scope", "none of the requested scopes is available to this account")
		return
	}

	code := randomString(secretBytes)
	err := s.store.CreateCode(ctx, &Code{
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
		s.fail(w, r, "store code", err)
		return
	}
	if err := s.store.DeleteRequest(ctx, req.ID); err != nil {
		s.log.WarnContext(ctx, "oauth: delete request", "err", err)
	}
	s.cookies.Delete(w, requestCookie)
	details := map[string]string{"client_id": req.ClientID, "scopes": strings.Join(a.scopes, " "), "via": a.via}
	if a.keyPrefix != "" {
		details["key_prefix"] = a.keyPrefix
	}
	s.record(ctx, a.handle, ActionGrant, a.handle, details)
	v := url.Values{"code": {code}, "iss": {s.opts.Issuer}}
	if req.ClientState != "" {
		v.Set("state", req.ClientState)
	}
	http.Redirect(w, r, withQuery(req.RedirectURI, v), http.StatusFound)
}

type approval struct {
	handle    string
	scopes    []string
	via       string // "oidc", "password" or "api_key"
	keyPrefix string // the approving API key, "" unless via is "api_key"
}

// approver is who approves the request. With an API key in the form it is
// the key's owner, and the grant is the key over OAuth: capped at its
// scopes, restricted to its slugs and alive only as long as it is. After a
// sign-in, at the identity provider or with a password, the key must be the
// signed-in person's own; without a key it is that person, with scopes
// capped by role. When ok is false a page has been written.
func (s *Service) approver(w http.ResponseWriter, r *http.Request, req *Request) (approval, bool) {
	ctx := r.Context()
	fail := func(status int, title, msg string) (approval, bool) {
		s.message(w, status, title, msg, "")
		return approval{}, false
	}
	var signedIn *user.User
	if req.UserHandle != "" {
		u, ok := s.signedIn(w, r, req.UserHandle)
		if !ok {
			return approval{}, false
		}
		signedIn = u
	} else if s.idp != nil {
		return fail(http.StatusForbidden, "Sign-in required",
			"Sign in at the identity provider first. Start again from your application.")
	}

	key := strings.TrimSpace(r.PostForm.Get("api_key"))
	if signedIn != nil && key == "" {
		via := signin.ViaOIDC
		if s.idp == nil {
			via = signin.ViaPassword
		}
		return approval{handle: signedIn.Handle, scopes: intersect(req.Scopes, user.ScopesFor(signedIn)), via: via}, true
	}
	if s.keys == nil {
		return fail(http.StatusForbidden, "Sign-in required", "This instance has no way to approve with an API key.")
	}
	p, err := s.signIn.CheckKey(ctx, key)
	if err == nil && signedIn != nil && p.Handle != signedIn.Handle {
		err = auth.ErrInvalidToken
	}
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidToken) {
			s.log.ErrorContext(ctx, "oauth: authenticate api key", "err", err)
		}
		c, cerr := s.store.GetClient(ctx, req.ClientID)
		if cerr != nil {
			s.pages.Failed(w)
			return approval{}, false
		}
		handle, scopes := "", req.Scopes
		if signedIn != nil {
			handle, scopes = signedIn.Handle, intersect(req.Scopes, user.ScopesFor(signedIn))
		}
		s.renderConsent(w, http.StatusUnauthorized, c, req, handle, scopes, "That API key was not accepted.")
		return approval{}, false
	}
	return approval{handle: p.Handle, scopes: intersect(req.Scopes, p.Scopes), via: signin.ViaAPIKey, keyPrefix: p.KeyID}, true
}
