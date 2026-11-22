package account

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/user"
)

// addPasskey keeps the passkey the browser made for the form's ceremony,
// once the person confirms it is them: a session alone adds no way to sign
// in.
func (s *server) addPasskey(w http.ResponseWriter, r *http.Request, v visit) {
	f := r.PostForm
	name := strings.TrimSpace(f.Get("name"))
	if s.refused(w, r, v, "passkey-confirm", accountPageData{PasskeyName: name}) {
		return
	}
	refuse := func(status int, why string) {
		s.renderAccount(w, r, status, v, accountPageData{Err: map[string]string{"passkey": why}, PasskeyName: name})
	}
	_, err := s.d.Passkeys.Register(r.Context(), v.user, f.Get("passkey_token"), f.Get("passkey"), name)
	switch {
	case errors.Is(err, passkey.ErrExpired), errors.Is(err, user.ErrPasskeyStale):
		refuse(http.StatusBadRequest, "expired")
	case errors.Is(err, passkey.ErrRejected):
		refuse(http.StatusUnprocessableEntity, "rejected")
	case errors.Is(err, user.ErrPasskeyExists):
		refuse(http.StatusConflict, "exists")
	case errors.Is(err, user.ErrTooManyPasskeys):
		refuse(http.StatusForbidden, "too-many")
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
