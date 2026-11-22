package account

import (
	"errors"
	"net/http"
	"strings"
	"time"

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
}

func (s *server) recoverPage(w http.ResponseWriter, r *http.Request) {
	s.renderRecover(w, r, http.StatusOK, recoverPageData{})
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
	d := recoverPageData{Login: strings.TrimSpace(f.Get("login"))}
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
	s.done(w, r, "password-recovered")
}
