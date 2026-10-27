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
	"github.com/PeterBooker/kazari/internal/user"
)

// Browser sign-in for gated artifacts. An artifact host sends a person to
// PathSignIn with its host label, the path to return to and the S256 hash
// of a nonce cookie it set. A browser with a live session cookie goes
// straight back with a single-use code; otherwise the person signs in at
// the identity provider first, or, without one, with an API key. The
// artifact host redeems the code in-process (Redeem) for a view session
// whose cookie only that host receives.
const (
	PathSignIn = "/oauth/signin"

	// Cookie names before the __Host- prefix: the session on the MCP host,
	// and the per-browser value that binds a pending identity-provider
	// sign-in to the browser that started it.
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

// signInTarget is where a browser sign-in returns: an artifact host label,
// its callback URL, a path on it and the hash of its nonce cookie.
type signInTarget struct {
	host, callback, path, nonceHash string
}

// target validates the parameters an artifact host sent. ok is false
// for a host that is not an artifact host or a malformed nonce hash; a
// path that is not a plain path on the host becomes "/".
func (s *Service) target(host, path, nonceHash string) (signInTarget, bool) {
	if s.opts.ViewCallback == nil || len(nonceHash) != nonceLen || !pkceValue.MatchString(nonceHash) {
		return signInTarget{}, false
	}
	cb, ok := s.opts.ViewCallback(host)
	if !ok {
		return signInTarget{}, false
	}
	return signInTarget{host: host, callback: cb, path: ReturnPath(path), nonceHash: nonceHash}, true
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

	if s.idp == nil {
		if s.keys == nil {
			s.message(w, http.StatusNotFound, "Sign-in unavailable", "This instance has no way to sign in.", "")
			return
		}
		s.renderSignIn(w, http.StatusOK, t, "")
		return
	}

	// The binding cookie is per browser, so several tabs signing in at
	// once each complete.
	binding := ""
	if ck, err := r.Cookie(s.named(signInCookie)); err == nil && len(ck.Value) == tokenLen {
		binding = ck.Value
	} else {
		binding = randomString(secretBytes)
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

// renderSignIn shows the API-key sign-in form, used without an identity
// provider.
func (s *Service) renderSignIn(w http.ResponseWriter, status int, t signInTarget, errMsg string) {
	site := t.callback
	if u, err := url.Parse(t.callback); err == nil {
		site = u.Scheme + "://" + u.Host
	}
	s.pages.render(w, status, "signin", signInPage{
		Site: site, Lifetime: lifetime(s.opts.SessionTTL), Host: t.host, Path: t.path, Nonce: t.nonceHash, Error: errMsg,
	}, t.callback)
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

// serveSignInKey signs a browser in with the API key from the sign-in
// form. Like the consent form, it must come from the form itself: an
// attacker's page could otherwise sign a visitor in as the attacker.
func (s *Service) serveSignInKey(w http.ResponseWriter, r *http.Request) {
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
	t, ok := s.target(f.Get("host"), f.Get("path"), f.Get("nonce"))
	if !ok {
		s.message(w, http.StatusBadRequest, "Invalid sign-in link",
			"This sign-in link is not valid. Open the page you wanted again.", "")
		return
	}
	if s.idp != nil || s.keys == nil {
		s.message(w, http.StatusForbidden, "Sign-in required",
			"Sign in at the identity provider. Open the page you wanted again.", "")
		return
	}
	p, err := s.keys.Authenticate(ctx, strings.TrimSpace(f.Get("api_key")))
	if err != nil || !p.HasScope(auth.ScopeRead) {
		if err != nil && !errors.Is(err, auth.ErrInvalidToken) {
			s.log.ErrorContext(ctx, "oauth: authenticate api key", "err", err)
		}
		s.renderSignIn(w, http.StatusUnauthorized, t, "That API key was not accepted.")
		return
	}
	sess, err := s.startSession(ctx, w, p.Handle, p.KeyID)
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: start session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.sendToHost(w, r, sess, t)
}

// serveSignInCallback finishes a browser sign-in at the identity provider.
func (s *Service) serveSignInCallback(w http.ResponseWriter, r *http.Request) {
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
		s.log.WarnContext(ctx, "oauth: identity provider exchange", "err", err)
		s.message(w, http.StatusBadGateway, "Sign-in failed",
			"The identity provider's response could not be verified. Open the page you wanted again.", "")
		return
	}
	u, err := s.users.ResolveIdentity(ctx, id)
	if errors.Is(err, user.ErrNotProvisioned) || errors.Is(err, user.ErrDisabled) {
		s.log.InfoContext(ctx, "oauth: identity not admitted", "issuer", id.Issuer, "subject", id.Subject, "email", id.Email, "err", err)
		s.message(w, http.StatusForbidden, "No access",
			"Your account is not allowed to use this Kazari instance. Ask your administrator for access.", id.Email)
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: resolve identity", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	sess, err := s.startSession(ctx, w, u.Handle, "")
	if err != nil {
		s.log.ErrorContext(ctx, "oauth: start session", "err", err)
		s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
		return
	}
	s.sendToHost(w, r, sess, t)
}

// startSession stores a session for handle and sets its cookie. A session
// opened with an API key ends no later than the key.
func (s *Service) startSession(ctx context.Context, w http.ResponseWriter, handle, keyPrefix string) (*Session, error) {
	now := s.now()
	expires := now.Add(s.opts.SessionTTL)
	via := "oidc"
	if keyPrefix != "" {
		via = "api_key"
		keyExpires, live, err := s.approvingKey(ctx, keyPrefix, handle)
		if err != nil {
			return nil, err
		}
		if !live {
			return nil, fmt.Errorf("oauth: key %s is not active", keyPrefix)
		}
		if keyExpires != nil && keyExpires.Before(expires) {
			expires = *keyExpires
		}
	}
	token := randomString(secretBytes)
	sess := &Session{Hash: hashSecret(token), UserHandle: handle, KeyPrefix: keyPrefix, CreatedAt: now, ExpiresAt: expires}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	s.setNamedCookie(w, s.named(sessionCookie), token, expires.Sub(now))
	details := map[string]string{"via": via}
	if keyPrefix != "" {
		details["key_prefix"] = keyPrefix
	}
	s.record(ctx, handle, ActionSignIn, details)
	return sess, nil
}

// liveSession returns the browser's session, or nil when it has none that
// is live: unexpired, its user enabled and, for an API-key sign-in, the key
// still active.
func (s *Service) liveSession(ctx context.Context, r *http.Request) (*Session, error) {
	ck, err := r.Cookie(s.named(sessionCookie))
	if err != nil || len(ck.Value) != tokenLen {
		return nil, nil
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
	_, live, err := s.approvingKey(ctx, keyPrefix, handle)
	return live, err
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
func (s *Service) Redeem(ctx context.Context, code, host, nonce string) (View, error) {
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

// SignOut ends the sign-in session behind the view session token on host,
// and with it every view session it opened, on every host. An unknown
// token is not an error.
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
