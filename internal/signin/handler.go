package signin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/theme"
	"github.com/PeterBooker/kazari/internal/user"
)

// Cookie names before the __Host- prefix: the session on the MCP host, and
// the per-browser value that binds a pending identity-provider sign-in to
// the browser that started it and links the sessions of one browser, so
// that sign-out ends them together.
const (
	sessionCookie = "kazari_session"
	signInCookie  = "kazari_signin"
)

const maxBodyBytes = 64 << 10

// RequestForm is the sign-in form for an MCP client's pending
// authorization, which goes on to the consent page.
type RequestForm struct {
	ID     string // the request
	Client string // the client's name
	// Bind starts the browser's sign-in, as the first page of one does;
	// otherwise the form is filled in from the query, as on a page that a
	// person comes back to.
	Bind bool
}

// Requests are the pending authorizations of MCP clients, which the
// sign-in form signs a person in for.
type Requests interface {
	// Form loads the pending request a posted form names, which must be
	// the browser's own. When ok is false it has answered.
	Form(w http.ResponseWriter, r *http.Request, id string) (f RequestForm, ok bool)
	// Continue binds request id to the person who signed in and sends the
	// browser on to the consent page.
	Continue(w http.ResponseWriter, r *http.Request, id, handle string)
}

// Mount adds the sign-in host's pages to mux, the MCP host's; a sign-in for
// an MCP client's authorization goes on through requests.
func (s *Service) Mount(mux *http.ServeMux, requests Requests) {
	// No method in the pattern: the theme answers 405 itself.
	mux.Handle(PathAssets, theme.Handler(PathAssets))
	mux.HandleFunc("GET "+PathSignIn, s.serveSignIn)
	mux.HandleFunc("POST "+PathSignIn, func(w http.ResponseWriter, r *http.Request) { s.serveSignInForm(w, r, requests) })
	mux.HandleFunc("GET "+PathSignOut, s.serveSignOutPage)
	mux.HandleFunc("POST "+PathSignOut, s.serveSignOut)
	if s.opts.RecoverURL != "" {
		// The sign-in page links here, so it names no other origin.
		mux.HandleFunc("GET "+PathRecover, s.serveRecover)
	}
}

// target is where a browser sign-in returns: a host label, its callback
// URL, a path on it and the hash of its nonce cookie.
type target struct {
	host, callback, path, nonceHash string
}

// target validates the parameters a host sent. ok is false for a label
// that names neither an artifact host nor a control host, or a malformed
// nonce hash; a path that is not a plain path on the host becomes "/".
func (s *Service) target(host, path, nonceHash string) (target, bool) {
	if !validNonce(nonceHash) {
		return target{}, false
	}
	callback := s.view
	if strings.HasPrefix(host, controlMark) {
		callback = s.control
	}
	if callback == nil {
		return target{}, false
	}
	cb, ok := callback(host)
	if !ok {
		return target{}, false
	}
	return target{host: host, callback: cb, path: ReturnPath(path), nonceHash: nonceHash}, true
}

// form returns the sign-in page for the target.
func (t target) form() signInPage {
	p := signInPage{Host: t.host, Path: t.path, Nonce: t.nonceHash, formTarget: t.callback}
	switch t.host {
	case HostAdmin:
		p.Title, p.Lede = "Sign in to the dashboard", "The dashboard is open to the admins of this Kazari instance."
	case HostAccount:
		p.Title, p.Lede = "Sign in to your account", "Your password, API keys and connected apps on this Kazari instance."
	default:
		site := t.callback
		if u, err := url.Parse(t.callback); err == nil {
			site = u.Scheme + "://" + u.Host
		}
		p.Title, p.Site = "Sign in to view this page", site
	}
	return p
}

// requestPage is the sign-in page that continues an MCP client's
// authorization.
func (s *Service) requestPage(f RequestForm) signInPage {
	return signInPage{
		Title:      "Sign in to connect " + f.Client,
		Lede:       "Sign in first; the next page asks what " + f.Client + " may do.",
		RequestID:  f.ID,
		KeyConsent: s.keys != nil,
	}
}

// ShowRequestForm writes the sign-in form that continues an MCP client's
// pending authorization.
func (s *Service) ShowRequestForm(w http.ResponseWriter, r *http.Request, f RequestForm) {
	page := s.requestPage(f)
	if f.Bind {
		s.cookies.Set(w, signInCookie, s.binding(r), s.opts.SessionTTL)
	} else {
		prefill(&page, r.URL.Query())
	}
	s.renderSignIn(w, r, http.StatusOK, page, false, "")
}

// serveSignIn starts a browser sign-in for a host.
func (s *Service) serveSignIn(w http.ResponseWriter, r *http.Request) {
	r, span := s.handle(r, "oauth.sign_in")
	defer span.End()
	ctx := r.Context()
	q := r.URL.Query()
	t, ok := s.target(q.Get("host"), q.Get("path"), q.Get("nonce"))
	if !ok {
		s.pages.Message(w, http.StatusBadRequest, "Invalid sign-in link",
			"This sign-in link is not valid. Open the page you wanted again.", "")
		return
	}
	if code := q.Get(ParamHandoff); code != "" {
		anchor, err := s.handoffAnchor(ctx, code, false)
		if err != nil {
			s.fail(w, r, "hand off", err)
			return
		}
		if anchor != nil {
			// A link can bring any browser here, so only the button,
			// posted from this page, signs it in.
			s.pages.Render(w, http.StatusOK, "handoff", handoffPage{
				Handle: anchor.UserHandle, Code: code, Host: t.host, Path: t.path, Nonce: t.nonceHash,
			}, t.callback)
			return
		}
	}
	sess, err := s.Session(r)
	if err != nil {
		s.fail(w, r, "load session", err)
		return
	}
	if sess != nil {
		s.sendToHost(w, r, sess, t)
		return
	}

	// The binding cookie is per browser, so several tabs signing in at
	// once each complete, and their sessions end together.
	binding := s.binding(r)
	if s.idp == nil {
		if s.keys == nil && s.passwords == nil {
			s.pages.Message(w, http.StatusNotFound, "Sign-in unavailable", "This instance has no way to sign in.", "")
			return
		}
		// The form has no expiry, so the cookie outlasts a pending sign-in.
		s.cookies.Set(w, signInCookie, binding, s.opts.SessionTTL)
		page := t.form()
		prefill(&page, q)
		s.renderSignIn(w, r, http.StatusOK, page, q.Get(ParamWith) == WithKey, "")
		return
	}

	if s.Unavailable(w, r) {
		return
	}
	verifier := oauth2.GenerateVerifier()
	req := &Request{
		ID:               StatePrefix + randomString(secretBytes),
		Binding:          hashSecret(binding),
		Nonce:            randomString(secretBytes),
		UpstreamVerifier: verifier,
		Host:             t.host,
		Path:             t.path,
		NonceHash:        t.nonceHash,
		ExpiresAt:        s.now().Add(s.opts.RequestTTL),
	}
	if err := s.store.CreateSignIn(ctx, req); err != nil {
		s.fail(w, r, "store sign-in", err)
		return
	}
	s.cookies.Set(w, signInCookie, binding, s.opts.RequestTTL)
	http.Redirect(w, r, s.idp.AuthURL(req.ID, req.Nonce, oauth2.S256ChallengeFromVerifier(verifier)), http.StatusFound)
}

// Unavailable reports, and answers, a sign-in the identity provider cannot
// take yet, such as before it has been reached once.
func (s *Service) Unavailable(w http.ResponseWriter, r *http.Request) bool {
	p, ok := s.idp.(interface{ Ready() error })
	if !ok {
		return false
	}
	err := p.Ready()
	if err == nil {
		return false
	}
	s.log.WarnContext(r.Context(), "signin: sign-in while the identity provider is unavailable", "err", err)
	w.Header().Set("Retry-After", "30")
	s.pages.Message(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable",
		"This instance cannot reach its identity provider yet. Please try again in a minute.", "")
	return true
}

// binding returns the browser's sign-in cookie value, or a new one when it
// has none.
func (s *Service) binding(r *http.Request) string {
	if v := s.cookies.Get(r, signInCookie); len(v) == tokenLen {
		return v
	}
	return randomString(secretBytes)
}

// renderSignIn shows the sign-in form, used without an identity provider:
// for a password where built-in accounts are on, else, or when byKey asks
// for it, for an API key. Each form links to the other when both exist.
func (s *Service) renderSignIn(w http.ResponseWriter, r *http.Request, status int, p signInPage, byKey bool, errMsg string) {
	p.Password = s.passwords != nil && !(byKey && s.keys != nil)
	p.Lifetime = lifetime(s.opts.SessionTTL)
	if s.opts.RecoverURL != "" {
		p.Recover = PathRecover
		if q := p.pending().Encode(); q != "" {
			p.Recover += "?" + q
		}
	}
	if p.Password && s.passkeys != nil {
		if c, err := s.passkeys.SignIn(); err != nil {
			s.log.ErrorContext(r.Context(), "signin: begin passkey sign-in", "err", err)
		} else {
			p.Passkey = &c
		}
	}
	p.Error = errMsg
	if s.passwords != nil && s.keys != nil && p.RequestID == "" {
		q := url.Values{"host": {p.Host}, "path": {p.Path}, "nonce": {p.Nonce}}
		if p.Password {
			q.Set(ParamWith, WithKey)
		}
		p.Switch = PathSignIn + "?" + q.Encode()
	}
	s.pages.Render(w, status, "signin", p, p.formTarget)
}

// pending names the sign-in the page continues, for a page that leaves it
// and comes back: the recovery form.
func (p signInPage) pending() url.Values {
	if p.RequestID != "" {
		return url.Values{"request_id": {p.RequestID}}
	}
	if p.Host == "" {
		return nil
	}
	return url.Values{"host": {p.Host}, "path": {p.Path}, "nonce": {p.Nonce}}
}

// serveRecover sends a browser on to the recovery form with the sign-in it
// came from, so that the form can return to it. Only a sign-in this page
// could continue is passed on.
func (s *Service) serveRecover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var carry url.Values
	if id := q.Get("request_id"); len(id) == tokenLen && nonceValue.MatchString(id) {
		carry = url.Values{"request_id": {id}}
	} else if t, ok := s.target(q.Get("host"), q.Get("path"), q.Get("nonce")); ok {
		carry = url.Values{"host": {t.host}, "path": {t.path}, "nonce": {t.nonceHash}}
	}
	to := s.opts.RecoverURL
	if len(carry) > 0 {
		to += "?" + carry.Encode()
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// prefill fills in what a link to the sign-in form may: the name to sign in
// with, and the notice of a fixed word.
func prefill(p *signInPage, q url.Values) {
	if login := strings.TrimSpace(q.Get("login")); len(login) <= user.MaxLoginLen {
		p.Login = login
	}
	if q.Get("notice") == noticeRecovered {
		p.Notice = noticeRecovered
	}
}

// noticeRecovered is the notice of a sign-in form reached from the recovery
// form, once the new password is set.
const noticeRecovered = "recovered"

// lifetime describes d in whole hours ("12 hours"), or minutes below one.
func lifetime(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	if h := int(d / time.Hour); h != 1 {
		return fmt.Sprintf("%d hours", h)
	}
	return "an hour"
}

// serveSignInForm signs a browser in with what the sign-in form sent: the
// name and password of a built-in account, a passkey, an API key, or the
// code of a sign-in another page handed over (ParamHandoff). Like
// the consent form, it must come from the form itself: an attacker's page
// could otherwise sign a visitor in as the attacker. The form names where
// the sign-in continues: a host, or the pending authorization of an MCP
// client (request_id), which then goes on to the consent page.
func (s *Service) serveSignInForm(w http.ResponseWriter, r *http.Request, requests Requests) {
	r, span := s.handle(r, "oauth.sign_in")
	defer span.End()
	ctx := r.Context()
	if !middleware.SameOrigin(r, s.opts.Origin) {
		s.pages.Message(w, http.StatusForbidden, "Request rejected",
			"This form was not sent from this server's sign-in page. Open the page you wanted again.", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.pages.Message(w, http.StatusBadRequest, "Bad request", "The form could not be read.", "")
		return
	}
	f := r.PostForm
	if code := f.Get(ParamHandoff); code != "" {
		s.serveHandoff(w, r, code)
		return
	}
	if s.idp != nil || (s.keys == nil && s.passwords == nil) {
		s.pages.Message(w, http.StatusForbidden, "Sign-in required",
			"Sign in at the identity provider. Open the page you wanted again.", "")
		return
	}

	var (
		page      signInPage
		t         target
		requestID string
	)
	if id := f.Get("request_id"); id != "" {
		if requests == nil {
			s.pages.Message(w, http.StatusForbidden, "Request rejected",
				"This form does not belong to the current sign-in. Start again from your application.", "")
			return
		}
		rf, ok := requests.Form(w, r, id)
		if !ok {
			return
		}
		requestID, page = rf.ID, s.requestPage(rf)
	} else {
		var ok bool
		if t, ok = s.target(f.Get("host"), f.Get("path"), f.Get("nonce")); !ok {
			s.pages.Message(w, http.StatusBadRequest, "Invalid sign-in link",
				"This sign-in link is not valid. Open the page you wanted again.", "")
			return
		}
		page = t.form()
	}

	var handle, keyPrefix, via string
	if _, byPasskey := f["passkey"]; byPasskey {
		u, ok := s.passkeySignIn(w, r, page)
		if !ok {
			return
		}
		handle, via = u.Handle, ViaPasskey
	} else if _, byKey := f["api_key"]; byKey {
		// An MCP client's authorization takes a key on the consent page,
		// where the key bounds the grant, not here.
		if s.keys == nil || requestID != "" {
			s.pages.Message(w, http.StatusBadRequest, "Bad request", "This form takes no API key.", "")
			return
		}
		p, err := s.CheckKey(ctx, strings.TrimSpace(f.Get("api_key")))
		if err != nil || !p.HasScope(auth.ScopeRead) {
			if err != nil && !errors.Is(err, auth.ErrInvalidToken) {
				s.log.ErrorContext(ctx, "signin: authenticate api key", "err", err)
			}
			s.renderSignIn(w, r, http.StatusUnauthorized, page, true, "That API key was not accepted.")
			return
		}
		// A session opens every members page, which is more than a restricted
		// key may read.
		if len(p.Slugs) > 0 {
			s.renderSignIn(w, r, http.StatusForbidden, page, true,
				"That API key is restricted to certain pages, so it cannot sign in. Use a key without a restriction.")
			return
		}
		handle, keyPrefix, via = p.Handle, p.KeyID, ViaAPIKey
	} else {
		u, ok := s.passwordSignIn(w, r, page)
		if !ok {
			return
		}
		handle, via = u.Handle, ViaPassword
	}

	sess, err := s.startSession(ctx, w, s.binding(r), handle, keyPrefix, via)
	if err != nil {
		s.fail(w, r, "start session", err)
		return
	}
	if requestID != "" {
		requests.Continue(w, r, requestID, handle)
		return
	}
	s.sendToHost(w, r, sess, t)
}

// passwordSignIn checks the name and password of the sign-in form. When ok
// is false it has answered: with the form again, and after too many
// failures with how long to wait. An unknown name is answered as a wrong
// password is, down to the delay it earns.
func (s *Service) passwordSignIn(w http.ResponseWriter, r *http.Request, page signInPage) (*user.User, bool) {
	ctx := r.Context()
	if s.passwords == nil {
		s.pages.Message(w, http.StatusForbidden, "Sign-in required", "This instance does not sign people in with a password.", "")
		return nil, false
	}
	login, password := strings.TrimSpace(r.PostForm.Get("login")), r.PostForm.Get("password")
	page.Login = login
	if login == "" || password == "" {
		s.renderSignIn(w, r, http.StatusBadRequest, page, false, "Enter your handle or email, and your password.")
		return nil, false
	}
	account := s.users.AccountKey(ctx, login)
	attempt, wait := ratelimit.BeginPair(ctx, s.accountDelay, account, s.ipDelay, ratelimit.IPKey(r))
	if wait > 0 {
		s.signedIn(ctx, ViaPassword, resultDelayed)
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		s.renderSignIn(w, r, http.StatusTooManyRequests, page, false,
			"Too many attempts. Try again in "+ratelimit.WaitText(wait)+".")
		return nil, false
	}
	u, err := s.passwords.CheckPassword(ctx, login, password)
	if errors.Is(err, user.ErrBadPassword) {
		attempt.Failed()
		s.signedIn(ctx, ViaPassword, resultFailed)
		s.renderSignIn(w, r, http.StatusUnauthorized, page, false, "That name or password was not accepted.")
		return nil, false
	}
	attempt.Done()
	if errors.Is(err, user.ErrBusy) {
		w.Header().Set("Retry-After", "5")
		s.renderSignIn(w, r, http.StatusServiceUnavailable, page, false, "The server is busy. Try again in a moment.")
		return nil, false
	}
	if err != nil {
		s.fail(w, r, "check password", err)
		return nil, false
	}
	s.accountDelay.Reset(account)
	s.signedIn(ctx, ViaPassword, resultOK)
	return u, true
}

// passkeySignIn checks the passkey the sign-in form carries. When ok is
// false it has answered, with the form again. A passkey names its person
// only once it verifies, so refusals are counted per address alone.
func (s *Service) passkeySignIn(w http.ResponseWriter, r *http.Request, page signInPage) (*user.User, bool) {
	ctx := r.Context()
	if s.passkeys == nil {
		s.pages.Message(w, http.StatusForbidden, "Sign-in required", "This instance does not sign people in with a passkey.", "")
		return nil, false
	}
	refuse := func(status int, msg string) {
		page.PasskeyError = msg
		s.renderSignIn(w, r, status, page, false, "")
	}
	attempt, wait := s.ipDelay.Begin(ratelimit.IPKey(r))
	if wait > 0 {
		s.ipDelay.Refused(ctx)
		s.signedIn(ctx, ViaPasskey, resultDelayed)
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		refuse(http.StatusTooManyRequests, "Too many attempts. Try again in "+ratelimit.WaitText(wait)+".")
		return nil, false
	}
	u, err := s.passkeys.Verify(ctx, r.PostForm.Get("passkey_token"), r.PostForm.Get("passkey"))
	switch {
	case errors.Is(err, passkey.ErrExpired):
		attempt.Done()
		s.signedIn(ctx, ViaPasskey, resultFailed)
		refuse(http.StatusBadRequest, "This page was open too long. Use your passkey again.")
		return nil, false
	case errors.Is(err, passkey.ErrRejected):
		attempt.Failed()
		s.signedIn(ctx, ViaPasskey, resultFailed)
		refuse(http.StatusUnauthorized, "That passkey was not accepted. It may have been removed from your account.")
		return nil, false
	case err != nil:
		attempt.Done()
		s.fail(w, r, "check passkey", err)
		return nil, false
	}
	attempt.Done()
	s.signedIn(ctx, ViaPasskey, resultOK)
	return u, true
}

// ServeCallback finishes a browser sign-in at the identity provider: the
// callback of a state that starts with StatePrefix.
func (s *Service) ServeCallback(w http.ResponseWriter, r *http.Request) {
	r, span := s.handle(r, "oauth.callback")
	defer span.End()
	ctx := r.Context()
	q := r.URL.Query()
	expired := func() {
		s.pages.Message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Open the page you wanted again.", "")
	}
	req, err := s.store.GetSignIn(ctx, q.Get("state"))
	if errors.Is(err, ErrNotFound) || err == nil && !s.now().Before(req.ExpiresAt) {
		expired()
		return
	}
	if err != nil {
		s.fail(w, r, "load sign-in", err)
		return
	}
	binding := s.cookies.Get(r, signInCookie)
	if subtle.ConstantTimeCompare([]byte(hashSecret(binding)), []byte(req.Binding)) != 1 {
		s.pages.Message(w, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response does not belong to this browser session. Open the page you wanted again.", "")
		return
	}
	// One use: of two callbacks racing for this sign-in, one goes on.
	if err := s.store.DeleteSignIn(ctx, req.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			expired()
			return
		}
		s.fail(w, r, "delete sign-in", err)
		return
	}
	if e := q.Get("error"); e != "" {
		s.pages.Message(w, http.StatusForbidden, "Sign-in failed",
			"The identity provider did not sign you in. Open the page you wanted again.", e)
		return
	}
	t, ok := s.target(req.Host, req.Path, req.NonceHash)
	if !ok {
		expired()
		return
	}
	u, ok := s.Identify(w, r, req.Nonce, req.UpstreamVerifier, "Open the page you wanted again.", nil)
	if !ok {
		return
	}
	sess, err := s.startSession(ctx, w, binding, u.Handle, "", ViaOIDC)
	if err != nil {
		s.fail(w, r, "start session", err)
		return
	}
	s.sendToHost(w, r, sess, t)
}

// Identify redeems the code of the identity provider's callback with the
// nonce and PKCE verifier its sign-in began with, and returns the person
// it signed in, counting the sign-in. When ok is false it has answered with
// a page whose advice is again; refused, if not nil, runs first when the
// person may not use this instance.
func (s *Service) Identify(w http.ResponseWriter, r *http.Request, nonce, verifier, again string, refused func()) (*user.User, bool) {
	ctx := r.Context()
	id, err := s.idp.Exchange(ctx, r.URL.Query().Get("code"), nonce, verifier)
	if err != nil {
		s.signedIn(ctx, ViaOIDC, resultFailed)
		s.log.WarnContext(ctx, "signin: identity provider exchange", "err", err)
		s.pages.Message(w, http.StatusBadGateway, "Sign-in failed",
			"The identity provider's response could not be verified. "+again, "")
		return nil, false
	}
	u, err := s.users.ResolveIdentity(ctx, id)
	if errors.Is(err, user.ErrNotProvisioned) || errors.Is(err, user.ErrDisabled) {
		s.log.InfoContext(ctx, "signin: identity not admitted", "issuer", id.Issuer, "subject", id.Subject, "email", id.Email, "err", err)
		s.signedIn(ctx, ViaOIDC, resultNotAdmitted)
		if refused != nil {
			refused()
		}
		s.pages.Message(w, http.StatusForbidden, "No access",
			"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", id.Email)
		return nil, false
	}
	if err != nil {
		s.fail(w, r, "resolve identity", err)
		return nil, false
	}
	s.signedIn(ctx, ViaOIDC, resultOK)
	return u, true
}

// startSession stores a session for handle in the browser that binding
// names and sets both cookies, the binding lasting as long as the session
// so that a tab finishing later joins it. via is how the person signed in.
// A session opened with an API key ends no later than the key.
func (s *Service) startSession(ctx context.Context, w http.ResponseWriter, binding, handle, keyPrefix, via string) (*Session, error) {
	now := s.now()
	expires := now.Add(s.opts.SessionTTL)
	if keyPrefix != "" {
		key, err := s.key(ctx, keyPrefix, handle)
		if err != nil {
			return nil, err
		}
		if !key.live {
			return nil, fmt.Errorf("signin: key %q is not active or is restricted", keyPrefix)
		}
		if key.expires != nil && key.expires.Before(expires) {
			expires = *key.expires
		}
	}
	token := randomString(secretBytes)
	sess := &Session{Hash: hashSecret(token), Browser: hashSecret(binding), UserHandle: handle, KeyPrefix: keyPrefix, CreatedAt: now, ExpiresAt: expires}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	s.cookies.Set(w, sessionCookie, token, expires.Sub(now))
	s.cookies.Set(w, signInCookie, binding, expires.Sub(now))
	details := map[string]string{"via": via}
	if keyPrefix != "" {
		details["key_prefix"] = keyPrefix
	}
	s.record(ctx, handle, ActionSignIn, handle, details)
	return sess, nil
}

// Session returns the browser's session, or nil when it has none that is
// live: unexpired, its person enabled and, for an API-key sign-in, the key
// still active.
func (s *Service) Session(r *http.Request) (*Session, error) {
	token := s.cookies.Get(r, sessionCookie)
	if len(token) != tokenLen {
		return nil, nil
	}
	sess, err := s.store.GetSession(r.Context(), hashSecret(token))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := s.live(r.Context(), sess.UserHandle, sess.KeyPrefix, sess.ExpiresAt, sess.UserDisabled)
	if err != nil || !key.live {
		return nil, err
	}
	return sess, nil
}

// sendToHost issues a code for the session and sends the browser to the
// host's callback with it.
func (s *Service) sendToHost(w http.ResponseWriter, r *http.Request, sess *Session, t target) {
	code := randomString(secretBytes)
	err := s.store.CreateViewCode(r.Context(), &ViewCode{
		Hash:      hashSecret(code),
		SessionID: sess.ID,
		Host:      t.host,
		Path:      t.path,
		NonceHash: t.nonceHash,
		ExpiresAt: s.now().Add(s.opts.ViewCodeTTL),
	})
	if err != nil {
		s.fail(w, r, "store view code", err)
		return
	}
	http.Redirect(w, r, withQuery(t.callback, url.Values{"code": {code}}), http.StatusFound)
}

// serveHandoff signs the browser in with a code from LinkCode, posted from
// the page that asks the person to continue, and sends it on to the form's
// target. A code that opens nothing leads to the sign-in page for the
// target, as one in the link does.
func (s *Service) serveHandoff(w http.ResponseWriter, r *http.Request, code string) {
	ctx := r.Context()
	f := r.PostForm
	t, ok := s.target(f.Get("host"), f.Get("path"), f.Get("nonce"))
	if !ok {
		s.pages.Message(w, http.StatusBadRequest, "Invalid sign-in link",
			"This sign-in link is not valid. Open the page you wanted again.", "")
		return
	}
	anchor, err := s.handoffAnchor(ctx, code, true)
	if err != nil {
		s.fail(w, r, "hand off", err)
		return
	}
	if anchor == nil {
		q := url.Values{"host": {t.host}, "path": {t.path}, "nonce": {t.nonceHash}}
		if login := f.Get("login"); login != "" {
			q.Set("login", login)
		}
		http.Redirect(w, r, PathSignIn+"?"+q.Encode(), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteSession(ctx, anchor.ID); err != nil {
		s.fail(w, r, "hand off", err)
		return
	}
	sess, err := s.startSession(ctx, w, s.binding(r), anchor.UserHandle, "", ViaLink)
	if err != nil {
		s.fail(w, r, "hand off", err)
		return
	}
	s.sendToHost(w, r, sess, t)
}

// handoffAnchor returns the session that holds the person of a code from
// LinkCode, using the code up when consume is set. It is nil when the code
// opens nothing: unknown, used, expired, not a LinkCode's, or its person
// can no longer sign in.
func (s *Service) handoffAnchor(ctx context.Context, code string, consume bool) (*Session, error) {
	if len(code) != tokenLen {
		return nil, nil
	}
	var (
		c   *ViewCode
		err error
	)
	if consume {
		c, err = s.store.ConsumeViewCode(ctx, hashSecret(code), s.now())
	} else {
		c, err = s.store.GetViewCode(ctx, hashSecret(code))
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCodeUsed) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if (!consume && c.UsedAt != nil) || c.Host != hostHandoff || !s.now().Before(c.ExpiresAt) {
		return nil, nil
	}
	anchor, err := s.store.GetSessionByID(ctx, c.SessionID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if key, err := s.live(ctx, anchor.UserHandle, "", anchor.ExpiresAt, anchor.UserDisabled); err != nil || !key.live {
		return nil, err
	}
	return anchor, nil
}

// serveSignOutPage asks a person to confirm ending the sign-in. It changes
// nothing: a link from an artifact can bring a browser here, but only the
// button ends anything.
func (s *Service) serveSignOutPage(w http.ResponseWriter, r *http.Request) {
	r, span := s.handle(r, "oauth.sign_out")
	defer span.End()
	sess, err := s.Session(r)
	if err != nil {
		s.fail(w, r, "load session", err)
		return
	}
	if sess == nil {
		s.pages.Message(w, http.StatusOK, "Not signed in", "This browser is not signed in to this Kazari instance.", "")
		return
	}
	s.pages.Render(w, http.StatusOK, "signout", nil, "")
}

// serveSignOut ends the browser's sign-in session, and with it every view
// session it opened, on every host. Like the consent form it must come from
// this server's own page, which no artifact can script: sibling hosts are
// same-site, so the session cookie alone would not show that.
func (s *Service) serveSignOut(w http.ResponseWriter, r *http.Request) {
	r, span := s.handle(r, "oauth.sign_out")
	defer span.End()
	ctx := r.Context()
	if !middleware.SameOrigin(r, s.opts.Origin) {
		s.pages.Message(w, http.StatusForbidden, "Request rejected",
			"This form was not sent from this server's sign-out page.", "")
		return
	}
	if token := s.cookies.Get(r, sessionCookie); len(token) == tokenLen {
		sess, err := s.store.GetSession(ctx, hashSecret(token))
		if err == nil {
			err = s.store.DeleteSession(ctx, sess.ID)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.fail(w, r, "end session", err)
			return
		}
	}
	s.cookies.Delete(w, sessionCookie)
	s.pages.Message(w, http.StatusOK, "Signed out", "You are signed out of this Kazari instance in this browser.", "")
}

// fail logs an error that is not the visitor's doing and answers with a
// page that says nothing about it.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), "signin: "+what, "err", err)
	s.pages.Failed(w)
}
