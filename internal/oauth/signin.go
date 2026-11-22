package oauth

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
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

// Browser sign-in for the other hosts. A host sends a person to PathSignIn
// with its label, the path to return to and the S256 hash of a nonce cookie
// it set. A browser with a live session cookie goes straight back with a
// single-use code; otherwise the person signs in first: at the identity
// provider, or without one with the password of their built-in account or
// an API key. The host redeems the code in-process (Redeem) for a view
// session whose cookie only that host receives.
//
// On an artifact host a sign-in ends at PathSignOut and nowhere else:
// member-authored script runs there, so an artifact host can end only its
// own view session (EndView). A control host runs Kazari's pages alone and
// ends the whole sign-in (SignOut).
const (
	PathSignIn  = "/oauth/signin"
	PathSignOut = "/oauth/signout"
	// PathRecover sends a browser on to Options.RecoverURL.
	PathRecover = "/oauth/recover"

	// The labels of the control hosts. "@" is in no artifact label, so
	// neither can be mistaken for an artifact host.
	HostAdmin   = "@admin"   // the dashboard, at admin.public_url
	HostAccount = "@account" // the account pages, on the apex
	controlMark = "@"

	// Cookie names before the __Host- prefix: the session on the MCP host,
	// and the per-browser value that binds a pending identity-provider
	// sign-in to the browser that started it and links the sessions of one
	// browser, so that sign-out ends them together.
	sessionCookie = "kazari_session"
	signInCookie  = "kazari_signin"

	// signInStatePrefix starts the upstream state of a browser sign-in, so
	// the shared callback can tell it from an MCP client's authorization.
	// "." is not in the base64url alphabet of the authorization's states.
	signInStatePrefix = "v."

	// maxReturnPath bounds the path an artifact host asks to return to.
	maxReturnPath = 2048

	// nonceLen is the length of an artifact host's nonce cookie and of its
	// S256 hash: 32 bytes, base64url.
	nonceLen = 43
)

// View is a redeemed code: the token of the new view session, when it
// ends, and the path on the artifact host to return to.
type View struct {
	Token     string
	ExpiresAt time.Time
	Path      string
}

// signInTarget is where a browser sign-in returns: a host label, its
// callback URL, a path on it and the hash of its nonce cookie.
type signInTarget struct {
	host, callback, path, nonceHash string
}

// target validates the parameters a host sent. ok is false for a label
// that names neither an artifact host nor a control host, or a malformed
// nonce hash; a path that is not a plain path on the host becomes "/".
func (s *Service) target(host, path, nonceHash string) (signInTarget, bool) {
	if len(nonceHash) != nonceLen || !pkceValue.MatchString(nonceHash) {
		return signInTarget{}, false
	}
	callback := s.opts.ViewCallback
	if strings.HasPrefix(host, controlMark) {
		callback = s.opts.ControlCallback
	}
	if callback == nil {
		return signInTarget{}, false
	}
	cb, ok := callback(host)
	if !ok {
		return signInTarget{}, false
	}
	return signInTarget{host: host, callback: cb, path: ReturnPath(path), nonceHash: nonceHash}, true
}

// form returns the sign-in page for the target.
func (t signInTarget) form() signInPage {
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

// ReturnPath returns p if it is a path on the same host ("/" not followed
// by a second "/", with no backslash or control character anywhere, at
// most 2048 bytes), else "/". It keeps a return path from naming another
// host. A backslash is refused wherever it is, because browsers read it as
// "/" and cleaning "/x/../\h" leaves "/\h"; a browser sends one as %5C, so
// no real path holds one.
func ReturnPath(p string) string {
	if p == "" || p[0] != '/' || len(p) > maxReturnPath || strings.HasPrefix(p, "//") {
		return "/"
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f || p[i] == '\\' {
			return "/"
		}
	}
	return p
}

// serveSignIn starts a browser sign-in for an artifact host.
func (s *Service) serveSignIn(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.sign_in")
	defer endHandle(span, out)
	ctx := r.Context()
	q := r.URL.Query()
	t, ok := s.target(q.Get("host"), q.Get("path"), q.Get("nonce"))
	if !ok {
		s.message(w, http.StatusBadRequest, "Invalid sign-in link",
			"This sign-in link is not valid. Open the page you wanted again.", "")
		return
	}
	sess, err := s.liveSession(ctx, r)
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: load session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if sess != nil {
		s.sendToHost(w, r, sess, t)
		return
	}

	// The binding cookie is per browser, so several tabs signing in at
	// once each complete, and their sessions end together.
	binding := s.signInBinding(r)
	if s.idp == nil {
		if s.keys == nil && s.passwords == nil {
			s.message(w, http.StatusNotFound, "Sign-in unavailable", "This instance has no way to sign in.", "")
			return
		}
		// The form has no expiry, so the cookie outlasts a pending sign-in.
		s.setNamedCookie(w, s.named(signInCookie), binding, s.opts.SessionTTL)
		s.renderSignIn(w, http.StatusOK, t.form(), q.Get("with") == withKey, "")
		return
	}

	verifier := oauth2.GenerateVerifier()
	req := &SignInRequest{
		ID:               signInStatePrefix + randomString(secretBytes),
		Binding:          hashSecret(binding),
		Nonce:            randomString(secretBytes),
		UpstreamVerifier: verifier,
		Host:             t.host,
		Path:             t.path,
		NonceHash:        t.nonceHash,
		ExpiresAt:        s.now().Add(s.opts.RequestTTL),
	}
	if err := s.store.CreateSignIn(ctx, req); err != nil {
		s.log.ErrorContext(ctx, "oauth: store sign-in", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.setNamedCookie(w, s.named(signInCookie), binding, s.opts.RequestTTL)
	http.Redirect(w, r, s.idp.AuthURL(req.ID, req.Nonce, oauth2.S256ChallengeFromVerifier(verifier)), http.StatusFound)
}

// signInBinding returns the browser's sign-in cookie value, or a new one
// when it has none.
func (s *Service) signInBinding(r *http.Request) string {
	if ck, err := r.Cookie(s.named(signInCookie)); err == nil && len(ck.Value) == tokenLen {
		return ck.Value
	}
	return randomString(secretBytes)
}

// withKey is the value of the "with" parameter that asks for the API-key
// form on an instance that also takes passwords.
const withKey = "key"

// renderSignIn shows the sign-in form, used without an identity provider:
// for a password where built-in accounts are on, else, or when byKey asks
// for it, for an API key. Each form links to the other when both exist.
func (s *Service) renderSignIn(w http.ResponseWriter, status int, p signInPage, byKey bool, errMsg string) {
	p.Password = s.passwords != nil && !(byKey && s.keys != nil)
	p.Lifetime = lifetime(s.opts.SessionTTL)
	if s.opts.RecoverURL != "" {
		p.Recover = PathRecover
	}
	if p.Password && s.passkeys != nil {
		if c, err := s.passkeys.SignIn(); err != nil {
			s.log.Error("oauth: begin passkey sign-in", "err", err)
		} else {
			p.Passkey = &c
		}
	}
	p.Error = errMsg
	if s.passwords != nil && s.keys != nil && p.RequestID == "" {
		q := url.Values{"host": {p.Host}, "path": {p.Path}, "nonce": {p.Nonce}}
		if p.Password {
			q.Set("with", withKey)
		}
		p.Switch = PathSignIn + "?" + q.Encode()
	}
	s.pages.render(w, status, "signin", p, p.formTarget)
}

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
// name and password of a built-in account, or an API key. Like the consent
// form, it must come from the form itself: an attacker's page could
// otherwise sign a visitor in as the attacker. The form names where the
// sign-in continues: a host, or the pending authorization of an MCP client
// (request_id), which then goes on to the consent page.
func (s *Service) serveSignInForm(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.sign_in")
	defer endHandle(span, out)
	ctx := r.Context()
	if !s.sameOrigin(r) {
		s.message(w, http.StatusForbidden, "Request rejected",
			"This form was not sent from this server's sign-in page. Open the page you wanted again.", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.message(w, http.StatusBadRequest, "Bad request", "The form could not be read.", "")
		return
	}
	f := r.PostForm
	if s.idp != nil || (s.keys == nil && s.passwords == nil) {
		s.message(w, http.StatusForbidden, "Sign-in required",
			"Sign in at the identity provider. Open the page you wanted again.", "")
		return
	}

	var (
		page signInPage
		t    signInTarget
		req  *Request
	)
	if id := f.Get("request_id"); id != "" {
		var ok bool
		if req, ok = s.formRequest(w, r, id); !ok {
			return
		}
		page = s.requestForm(ctx, req)
	} else {
		var ok bool
		if t, ok = s.target(f.Get("host"), f.Get("path"), f.Get("nonce")); !ok {
			s.message(w, http.StatusBadRequest, "Invalid sign-in link",
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
		handle, via = u.Handle, viaPasskey
	} else if _, byKey := f["api_key"]; byKey {
		// An MCP client's authorization takes a key on the consent page,
		// where the key bounds the grant, not here.
		if s.keys == nil || req != nil {
			s.message(w, http.StatusBadRequest, "Bad request", "This form takes no API key.", "")
			return
		}
		p, err := s.keys.Authenticate(ctx, strings.TrimSpace(f.Get("api_key")))
		s.keySignIn(ctx, err)
		if err != nil || !p.HasScope(auth.ScopeRead) {
			if err != nil && !errors.Is(err, auth.ErrInvalidToken) {
				s.log.ErrorContext(ctx, "oauth: authenticate api key", "err", err)
			}
			s.renderSignIn(w, http.StatusUnauthorized, page, true, "That API key was not accepted.")
			return
		}
		// A session opens every members page, which is more than a restricted
		// key may read.
		if len(p.Slugs) > 0 {
			s.renderSignIn(w, http.StatusForbidden, page, true,
				"That API key is restricted to certain pages, so it cannot sign in. Use a key without a restriction.")
			return
		}
		handle, keyPrefix, via = p.Handle, p.KeyID, viaAPIKey
	} else {
		u, ok := s.passwordSignIn(w, r, page)
		if !ok {
			return
		}
		handle, via = u.Handle, viaPassword
	}

	sess, err := s.startSession(ctx, w, s.signInBinding(r), handle, keyPrefix, via)
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: start session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if req != nil {
		s.continueRequest(w, r, req, handle)
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
		s.message(w, http.StatusForbidden, "Sign-in required", "This instance does not sign people in with a password.", "")
		return nil, false
	}
	login, password := strings.TrimSpace(r.PostForm.Get("login")), r.PostForm.Get("password")
	page.Login = login
	if login == "" || password == "" {
		s.renderSignIn(w, http.StatusBadRequest, page, false, "Enter your handle or email, and your password.")
		return nil, false
	}
	account := s.accountKey(ctx, login)
	attempt, wait := ratelimit.BeginPair(ctx, s.accountDelay, account, s.ipDelay, ratelimit.IPKey(r))
	if wait > 0 {
		s.signedIn(ctx, viaPassword, signInDelayed)
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		s.renderSignIn(w, http.StatusTooManyRequests, page, false,
			"Too many attempts. Try again in "+waitText(wait)+".")
		return nil, false
	}
	u, err := s.passwords.CheckPassword(ctx, login, password)
	if errors.Is(err, user.ErrBadPassword) {
		attempt.Failed()
		s.signedIn(ctx, viaPassword, signInFailed)
		s.renderSignIn(w, http.StatusUnauthorized, page, false, "That name or password was not accepted.")
		return nil, false
	}
	attempt.Done()
	if errors.Is(err, user.ErrBusy) {
		w.Header().Set("Retry-After", "5")
		s.renderSignIn(w, http.StatusServiceUnavailable, page, false, "The server is busy. Try again in a moment.")
		return nil, false
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: check password", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return nil, false
	}
	s.accountDelay.Reset(account)
	s.signedIn(ctx, viaPassword, signInOK)
	return u, true
}

// passkeySignIn checks the passkey the sign-in form carries. When ok is
// false it has answered, with the form again. A passkey names its person
// only once it verifies, so refusals are counted per address alone.
func (s *Service) passkeySignIn(w http.ResponseWriter, r *http.Request, page signInPage) (*user.User, bool) {
	ctx := r.Context()
	if s.passkeys == nil {
		s.message(w, http.StatusForbidden, "Sign-in required", "This instance does not sign people in with a passkey.", "")
		return nil, false
	}
	refuse := func(status int, msg string) {
		page.PasskeyError = msg
		s.renderSignIn(w, status, page, false, "")
	}
	attempt, wait := s.ipDelay.Begin(ratelimit.IPKey(r))
	if wait > 0 {
		s.ipDelay.Refused(ctx)
		s.signedIn(ctx, viaPasskey, signInDelayed)
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		refuse(http.StatusTooManyRequests, "Too many attempts. Try again in "+waitText(wait)+".")
		return nil, false
	}
	u, err := s.passkeys.Verify(ctx, r.PostForm.Get("passkey_token"), r.PostForm.Get("passkey"))
	switch {
	case errors.Is(err, passkey.ErrExpired):
		attempt.Done()
		s.signedIn(ctx, viaPasskey, signInFailed)
		refuse(http.StatusBadRequest, "This page was open too long. Use your passkey again.")
		return nil, false
	case errors.Is(err, passkey.ErrRejected):
		attempt.Failed()
		s.signedIn(ctx, viaPasskey, signInFailed)
		refuse(http.StatusUnauthorized, "That passkey was not accepted. It may have been removed from your account.")
		return nil, false
	case err != nil:
		attempt.Done()
		s.log.ErrorContext(ctx, "oauth: check passkey", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return nil, false
	}
	attempt.Done()
	s.signedIn(ctx, viaPasskey, signInOK)
	return u, true
}

// accountKey is what failed sign-ins are counted under: the person the name
// belongs to, so that their handle and their email are one account, else
// the name as typed, folded and bounded, so an unknown name earns its
// delays like a real one.
func (s *Service) accountKey(ctx context.Context, login string) string {
	if handle := s.users.HandleFor(ctx, login); handle != "" {
		return handle
	}
	login = strings.ToLower(login)
	if len(login) > maxAccountKey {
		login = login[:maxAccountKey]
	}
	return login
}

// maxAccountKey is the longest email address (RFC 5321).
const maxAccountKey = 254

// waitText describes a wait in whole seconds, rounded up.
func waitText(d time.Duration) string {
	if n := ratelimit.RetryAfter(d); n != "1" {
		return n + " seconds"
	}
	return "a second"
}

// serveSignInCallback finishes a browser sign-in at the identity provider.
func (s *Service) serveSignInCallback(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.callback")
	defer endHandle(span, out)
	ctx := r.Context()
	q := r.URL.Query()
	expired := func() {
		s.message(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in is no longer active. Open the page you wanted again.", "")
	}
	req, err := s.store.GetSignIn(ctx, q.Get("state"))
	if errors.Is(err, ErrNotFound) || err == nil && !s.now().Before(req.ExpiresAt) {
		expired()
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: load sign-in", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	ck, err := r.Cookie(s.named(signInCookie))
	if err != nil || subtle.ConstantTimeCompare([]byte(hashSecret(ck.Value)), []byte(req.Binding)) != 1 {
		s.message(w, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response does not belong to this browser session. Open the page you wanted again.", "")
		return
	}
	// One use: of two callbacks racing for this sign-in, one goes on.
	if err := s.store.DeleteSignIn(ctx, req.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			expired()
			return
		}
		s.log.ErrorContext(ctx, "oauth: delete sign-in", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if e := q.Get("error"); e != "" {
		s.message(w, http.StatusForbidden, "Sign-in failed",
			"The identity provider did not sign you in. Open the page you wanted again.", e)
		return
	}
	t, ok := s.target(req.Host, req.Path, req.NonceHash)
	if !ok {
		expired()
		return
	}

	id, err := s.idp.Exchange(ctx, q.Get("code"), req.Nonce, req.UpstreamVerifier)
	if err != nil {
		s.signedIn(ctx, viaOIDC, signInFailed)
		s.log.WarnContext(ctx, "oauth: identity provider exchange", "err", err)
		s.message(w, http.StatusBadGateway, "Sign-in failed",
			"The identity provider's response could not be verified. Open the page you wanted again.", "")
		return
	}
	u, err := s.users.ResolveIdentity(ctx, id)
	if errors.Is(err, user.ErrNotProvisioned) || errors.Is(err, user.ErrDisabled) {
		s.log.InfoContext(ctx, "oauth: identity not admitted", "issuer", id.Issuer, "subject", id.Subject, "email", id.Email, "err", err)
		s.signedIn(ctx, viaOIDC, signInNotAdmitted)
		s.message(w, http.StatusForbidden, "No access",
			"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", id.Email)
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: resolve identity", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.signedIn(ctx, viaOIDC, signInOK)
	sess, err := s.startSession(ctx, w, ck.Value, u.Handle, "", viaOIDC)
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: start session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.sendToHost(w, r, sess, t)
}

// startSession stores a session for handle in the browser that binding
// names and sets both cookies, the binding lasting as long as the session
// so that a tab finishing later joins it. via is how the person signed in.
// A session opened with an API key ends no later than the key.
func (s *Service) startSession(ctx context.Context, w http.ResponseWriter, binding, handle, keyPrefix, via string) (*Session, error) {
	now := s.now()
	expires := now.Add(s.opts.SessionTTL)
	if keyPrefix != "" {
		key, err := s.approvingKey(ctx, keyPrefix, handle)
		if err != nil {
			return nil, err
		}
		if !key.live || len(key.slugs) > 0 {
			return nil, fmt.Errorf("oauth: key %q is not active or is restricted", keyPrefix)
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
	s.setNamedCookie(w, s.named(sessionCookie), token, expires.Sub(now))
	s.setNamedCookie(w, s.named(signInCookie), binding, expires.Sub(now))
	details := map[string]string{"via": via}
	if keyPrefix != "" {
		details["key_prefix"] = keyPrefix
	}
	s.record(ctx, handle, ActionSignIn, handle, details)
	return sess, nil
}

// liveSession returns the browser's session, or nil when it has none that
// is live: unexpired, its user enabled and, for an API-key sign-in, the key
// still active.
func (s *Service) liveSession(ctx context.Context, r *http.Request) (*Session, error) {
	ck, err := r.Cookie(s.named(sessionCookie))
	if err != nil || len(ck.Value) != tokenLen {
		return nil, nil //nolint:nilerr // the only error is http.ErrNoCookie, and no cookie is no session
	}
	sess, err := s.store.GetSession(ctx, hashSecret(ck.Value))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live, err := s.sessionLive(ctx, sess.UserHandle, sess.KeyPrefix, sess.ExpiresAt)
	if err != nil || !live {
		return nil, err
	}
	return sess, nil
}

// sessionLive is the check every use of a session or view session makes.
func (s *Service) sessionLive(ctx context.Context, handle, keyPrefix string, expires time.Time) (bool, error) {
	if !s.now().Before(expires) {
		return false, nil
	}
	u, err := s.users.GetUser(ctx, handle)
	if errors.Is(err, user.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if u.DisabledAt != nil {
		return false, nil
	}
	key, err := s.approvingKey(ctx, keyPrefix, handle)
	return key.live && len(key.slugs) == 0, err
}

// sendToHost issues a code for the session and sends the browser to the
// artifact host's callback with it.
func (s *Service) sendToHost(w http.ResponseWriter, r *http.Request, sess *Session, t signInTarget) {
	ctx := r.Context()
	code := randomString(secretBytes)
	err := s.store.CreateViewCode(ctx, &ViewCode{
		Hash:      hashSecret(code),
		SessionID: sess.ID,
		Host:      t.host,
		Path:      t.path,
		NonceHash: t.nonceHash,
		ExpiresAt: s.now().Add(s.opts.ViewCodeTTL),
	})
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: store view code", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	http.Redirect(w, r, withQuery(t.callback, url.Values{"code": {code}}), http.StatusFound)
}

// Redeem exchanges a code from a sign-in redirect for a view session on
// host. nonce is the value of the host's nonce cookie, whose S256 the code
// carries, so a code only works in the browser that asked for it. The view
// session ends with its sign-in session. ErrNotFound for an unknown, used,
// expired or foreign code, a nonce that does not match, or a session that
// is no longer live; the code is used up in every case. A missing or
// malformed nonce never matches: a code made for the S256 of "" would
// otherwise sign in any browser that has no nonce cookie, as whoever made
// it.
func (s *Service) Redeem(ctx context.Context, code, host, nonce string) (_ View, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "oauth.redeem")
	defer func() { instrument.End(span, err, redeemCode(err)) }()
	if len(code) != tokenLen {
		return View{}, ErrNotFound
	}
	c, err := s.store.ConsumeViewCode(ctx, hashSecret(code), s.now())
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCodeUsed) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("oauth: redeem: %w", err)
	}
	if !s.now().Before(c.ExpiresAt) || c.Host != host || len(nonce) != nonceLen || !pkceValue.MatchString(nonce) ||
		subtle.ConstantTimeCompare([]byte(s256(nonce)), []byte(c.NonceHash)) != 1 {
		return View{}, ErrNotFound
	}
	sess, err := s.store.GetSessionByID(ctx, c.SessionID)
	if errors.Is(err, ErrNotFound) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("oauth: redeem: %w", err)
	}
	live, err := s.sessionLive(ctx, sess.UserHandle, sess.KeyPrefix, sess.ExpiresAt)
	if err != nil {
		return View{}, fmt.Errorf("oauth: redeem: %w", err)
	}
	if !live {
		return View{}, ErrNotFound
	}
	token := randomString(secretBytes)
	v := &ViewSession{Hash: hashSecret(token), SessionID: sess.ID, Host: host, CreatedAt: s.now(), ExpiresAt: sess.ExpiresAt}
	if err := s.store.CreateViewSession(ctx, v); err != nil {
		return View{}, fmt.Errorf("oauth: redeem: %w", err)
	}
	return View{Token: token, ExpiresAt: v.ExpiresAt, Path: c.Path}, nil
}

// Viewer returns the handle of the person whose view session on host
// token is. ErrNotFound unless the view session exists for that host, has
// not expired and its sign-in session is still live.
func (s *Service) Viewer(ctx context.Context, host, token string) (string, error) {
	v, err := s.viewSession(ctx, host, token)
	if err != nil {
		return "", err
	}
	live, err := s.sessionLive(ctx, v.UserHandle, v.KeyPrefix, v.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("oauth: viewer: %w", err)
	}
	if !live {
		return "", ErrNotFound
	}
	return v.UserHandle, nil
}

// ControlSession is a person signed in on a control host.
type ControlSession struct {
	Handle string
	// KeyPrefix is the API key the person signed in with; "" after a
	// password or an identity-provider sign-in.
	KeyPrefix string
	// SessionID is the sign-in session, which every host it opened shares.
	SessionID int64
	ExpiresAt time.Time
}

// ControlSession returns who the view session token on the control host is,
// and how they signed in. ErrNotFound unless the view session exists for
// that host, has not expired and its sign-in session is still live: the
// person enabled and, for an API-key sign-in, the key still active. What
// the person may do there is the host's to decide, on every request.
func (s *Service) ControlSession(ctx context.Context, host, token string) (ControlSession, error) {
	v, err := s.viewSession(ctx, host, token)
	if err != nil {
		return ControlSession{}, err
	}
	live, err := s.sessionLive(ctx, v.UserHandle, v.KeyPrefix, v.ExpiresAt)
	if err != nil {
		return ControlSession{}, fmt.Errorf("oauth: control session: %w", err)
	}
	if !live {
		return ControlSession{}, ErrNotFound
	}
	return ControlSession{Handle: v.UserHandle, KeyPrefix: v.KeyPrefix, SessionID: v.SessionID, ExpiresAt: v.ExpiresAt}, nil
}

// SignOut ends the sign-in behind the view session token on the control
// host: the session, every session of the same browser and every view
// session they opened, on every host. An unknown token is not an error.
func (s *Service) SignOut(ctx context.Context, host, token string) error {
	v, err := s.viewSession(ctx, host, token)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.DeleteSession(ctx, v.SessionID)
}

// EndView ends the view session token on host. Its sign-in session and
// the view sessions of other hosts stay. An unknown token is not an error.
func (s *Service) EndView(ctx context.Context, host, token string) error {
	v, err := s.viewSession(ctx, host, token)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.DeleteViewSession(ctx, v.Hash)
}

// serveSignOutPage asks a person to confirm ending the sign-in. It changes
// nothing: a link from an artifact can bring a browser here, but only the
// button ends anything.
func (s *Service) serveSignOutPage(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.sign_out")
	defer endHandle(span, out)
	sess, err := s.liveSession(r.Context(), r)
	if err != nil {
		s.log.ErrorContext(r.Context(), "oauth: load session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	if sess == nil {
		s.message(w, http.StatusOK, "Not signed in", "This browser is not signed in to this Kazari instance.", "")
		return
	}
	s.pages.render(w, http.StatusOK, "signout", nil, "")
}

// serveSignOut ends the browser's sign-in session, and with it every view
// session it opened, on every host. Like the consent form it must come from
// this server's own page, which no artifact can script: sibling hosts are
// same-site, so the session cookie alone would not show that.
func (s *Service) serveSignOut(w http.ResponseWriter, r *http.Request) {
	r, out, span := s.handle(r, "oauth.sign_out")
	defer endHandle(span, out)
	ctx := r.Context()
	if !s.sameOrigin(r) {
		s.message(w, http.StatusForbidden, "Request rejected",
			"This form was not sent from this server's sign-out page.", "")
		return
	}
	if ck, err := r.Cookie(s.named(sessionCookie)); err == nil && len(ck.Value) == tokenLen {
		sess, err := s.store.GetSession(ctx, hashSecret(ck.Value))
		if err == nil {
			err = s.store.DeleteSession(ctx, sess.ID)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.ErrorContext(ctx, "oauth: end session", "err", err)
			s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
			return
		}
	}
	s.setNamedCookie(w, s.named(sessionCookie), "", -1)
	s.message(w, http.StatusOK, "Signed out", "You are signed out of this Kazari instance in this browser.", "")
}

// viewSession loads the view session token names on host. ErrNotFound.
func (s *Service) viewSession(ctx context.Context, host, token string) (*ViewSession, error) {
	if len(token) != tokenLen {
		return nil, ErrNotFound
	}
	v, err := s.store.GetViewSession(ctx, hashSecret(token))
	if err != nil {
		return nil, err
	}
	if v.Host != host {
		return nil, ErrNotFound
	}
	return v, nil
}

// redeemCode keeps an unknown or expired code, a stale browser tab, out of
// the internal errors.
func redeemCode(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "not_found"
	}
	return ""
}
