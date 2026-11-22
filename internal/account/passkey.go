package account

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

// addPasskey keeps the passkey the browser made for the form's ceremony,
// given the current password: a session alone adds no way to sign in.
func (s *server) addPasskey(w http.ResponseWriter, r *http.Request, v visit) {
	f := r.PostForm
	name := strings.TrimSpace(f.Get("name"))
	refuse := func(status int, why string, wait time.Duration) {
		d := accountPageData{Err: map[string]string{"passkey": why}, PasskeyName: name}
		if wait > 0 {
			w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
			d.Seconds = max(1, int((wait+time.Second-1)/time.Second))
		}
		s.renderAccount(w, r, status, v, d)
	}
	attempt, wait := ratelimit.BeginPair(r.Context(), s.passwordDelay, v.user.Handle, s.addressDelay, ratelimit.IPKey(r))
	if wait > 0 {
		refuse(http.StatusTooManyRequests, "wait", wait)
		return
	}
	err := s.d.Users.ConfirmPassword(r.Context(), v.user.Handle, f.Get("current"))
	if errors.Is(err, user.ErrBadPassword) {
		attempt.Failed()
	} else {
		attempt.Done()
	}
	switch {
	case errors.Is(err, user.ErrBadPassword):
		refuse(http.StatusUnprocessableEntity, "wrong", 0)
		return
	case errors.Is(err, user.ErrBusy):
		s.busy(w, r)
		return
	case err != nil:
		s.fail(w, r, "confirm password", err)
		return
	}
	s.passwordDelay.Reset(v.user.Handle)

	_, err = s.d.Passkeys.Register(r.Context(), v.user, f.Get("passkey_token"), f.Get("passkey"), name)
	switch {
	case errors.Is(err, passkey.ErrExpired), errors.Is(err, user.ErrPasskeyStale):
		refuse(http.StatusBadRequest, "expired", 0)
	case errors.Is(err, passkey.ErrRejected):
		refuse(http.StatusUnprocessableEntity, "rejected", 0)
	case errors.Is(err, user.ErrPasskeyExists):
		refuse(http.StatusConflict, "exists", 0)
	case errors.Is(err, user.ErrTooManyPasskeys):
		refuse(http.StatusForbidden, "too-many", 0)
	case err != nil:
		s.fail(w, r, "add passkey", err)
	default:
		s.done(w, r, "passkey-added")
	}
}

// removePasskey deletes one of the person's passkeys. Another person's, like
// one that does not exist, is not found.
func (s *server) removePasskey(w http.ResponseWriter, r *http.Request, v visit) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		s.message(w, r, http.StatusNotFound, "not-found")
		return
	}
	err = s.d.Users.RemovePasskey(r.Context(), v.user.Handle, id)
	switch {
	case errors.Is(err, user.ErrPasskeyNotFound):
		s.message(w, r, http.StatusNotFound, "not-found")
	case err != nil:
		s.fail(w, r, "remove passkey", err)
	default:
		s.done(w, r, "passkey-removed")
	}
}
