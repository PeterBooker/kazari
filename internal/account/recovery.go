package account

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

// recoveryPage shows a new set of recovery codes, once.
type recoveryPage struct {
	base
	Codes []string
}

// newRecoveryCodes makes a new set of recovery codes, once the person
// confirms it is them, and shows them in the answer to the POST.
func (s *server) newRecoveryCodes(w http.ResponseWriter, r *http.Request, v visit) {
	if s.refused(w, r, v, "recovery", accountPageData{}) {
		return
	}
	codes, err := s.d.Users.NewRecoveryCodes(r.Context(), v.user.Handle)
	if err != nil {
		s.fail(w, r, "make recovery codes", err)
		return
	}
	s.render(w, r, http.StatusOK, "recovery", recoveryPage{base: s.baseFor(v), Codes: codes})
}

// maxLoginLen is the longest email address (RFC 5321).
const maxLoginLen = 254

// recoverPageData is the form that uses a recovery code.
type recoverPageData struct {
	base
	MinLength, MaxLength int
	Login                string
	Err                  string
	Seconds              int
	// Pending is the sign-in the person left for this form, carried in its
	// hidden fields and continued once the password is set.
	Pending pendingSignIn
}

// pendingSignIn is a sign-in on the sign-in host: an MCP client's
// authorization (RequestID) or a host's sign-in (Host, Path, Nonce). The
// sign-in host checks it again; here it is only bounded.
type pendingSignIn struct {
	RequestID, Host, Path, Nonce string
}

// maxPendingField bounds each field of a pending sign-in; the longest is a
// path, which the sign-in host bounds the same.
const maxPendingField = 2048

func readPending(v url.Values) pendingSignIn {
	get := func(k string) string {
		if s := v.Get(k); len(s) <= maxPendingField {
			return s
		}
		return ""
	}
	if id := get("request_id"); id != "" {
		return pendingSignIn{RequestID: id}
	}
	p := pendingSignIn{Host: get("host"), Path: get("path"), Nonce: get("nonce")}
	if p.Host == "" || p.Nonce == "" {
		return pendingSignIn{}
	}
	return p
}

// next is where the browser goes once the password is set: back to the
// sign-in it left, with the name filled in, or "" when it left none.
func (s *server) next(p pendingSignIn, login string) string {
	q := url.Values{"login": {login}, "notice": {"recovered"}}
	switch {
	case p.RequestID != "":
		return strings.TrimSuffix(s.o.SignInURL, oauth.PathSignIn) + oauth.PathConsent + "?" + q.Encode()
	case p.Host != "":
		q.Set("host", p.Host)
		q.Set("path", p.Path)
		q.Set("nonce", p.Nonce)
		return s.o.SignInURL + "?" + q.Encode()
	}
	return ""
}

func (s *server) recoverPage(w http.ResponseWriter, r *http.Request) {
	s.renderRecover(w, r, http.StatusOK, recoverPageData{Pending: readPending(r.URL.Query())})
}

func (s *server) renderRecover(w http.ResponseWriter, r *http.Request, status int, d recoverPageData) {
	d.base = s.base()
	d.MinLength = s.d.Users.MinPasswordLength()
	d.MaxLength = user.MaxPasswordLength
	// Browsers hold the submission to form-action through the redirects
	// that follow it, and they end on the sign-in host.
	w.Header().Set("Content-Security-Policy", s.recoverCSP)
	s.render(w, r, status, "recover", d)
}

// recover sets a new password with a recovery code. Wrong codes slow the
// next attempt as wrong passwords do, counted for the person the name
// belongs to and for the address. Nobody is signed in by it; the next page
// is the sign-in form.
func (s *server) recover(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.message(w, r, http.StatusBadRequest, "bad-form")
		return
	}
	f := r.PostForm
	d := recoverPageData{Login: strings.TrimSpace(f.Get("login")), Pending: readPending(f)}
	refuse := func(status int, why string) {
		d.Err = why
		s.renderRecover(w, r, status, d)
	}
	if d.Login == "" || f.Get("code") == "" {
		refuse(http.StatusUnprocessableEntity, "missing")
		return
	}
	if f.Get("password") != f.Get("confirm") {
		refuse(http.StatusUnprocessableEntity, "mismatch")
		return
	}
	// Counted for the person the name belongs to, so their handle and
	// email are one account, else for the name as typed, bounded.
	account := s.d.Users.HandleFor(r.Context(), d.Login)
	if account == "" {
		account = strings.ToLower(d.Login)
		if len(account) > maxLoginLen {
			account = account[:maxLoginLen]
		}
	}
	attempt, wait := ratelimit.BeginPair(r.Context(), s.passwordDelay, account, s.addressDelay, ratelimit.IPKey(r))
	if wait > 0 {
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		d.Seconds = max(1, int((wait+time.Second-1)/time.Second))
		refuse(http.StatusTooManyRequests, "wait")
		return
	}
	_, err := s.d.Users.Recover(r.Context(), d.Login, f.Get("code"), f.Get("password"))
	if errors.Is(err, user.ErrBadRecoveryCode) {
		attempt.Failed()
	} else {
		attempt.Done()
	}
	switch {
	case errors.Is(err, user.ErrBadRecoveryCode):
		refuse(http.StatusUnprocessableEntity, "wrong")
		return
	case errors.Is(err, user.ErrWeakPassword):
		refuse(http.StatusUnprocessableEntity, "weak")
		return
	case errors.Is(err, user.ErrBusy):
		s.busy(w, r)
		return
	case err != nil:
		s.fail(w, r, "recover", err)
		return
	}
	s.passwordDelay.Reset(account)
	if to := s.next(d.Pending, d.Login); to != "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	s.done(w, r, "password-recovered")
}
