package account

import (
	"errors"
	"net/http"
	"time"

	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

// A form that changes how a person signs in asks them to confirm it is
// them, so that a browser left signed in cannot: with their current
// password or, with that field left empty, with one of their passkeys.

// Why a confirmation was refused. The templates have the words.
const (
	confirmWrong   = "wrong"   // not the current password
	confirmPasskey = "passkey" // a passkey that was not accepted
	confirmExpired = "expired" // the page's passkey ceremony is not live
	confirmMissing = "missing" // neither a password nor a passkey
	confirmWait    = "wait"    // too soon after failures
)

// confirm checks the form's confirmation. why is "" when the person is
// confirmed, else what was refused, with wait the delay of confirmWait. err
// is a failure that is not the person's doing, user.ErrBusy among them.
// Wrong passwords and refused passkeys slow the next attempt alike.
func (s *server) confirm(r *http.Request, v visit) (why string, wait time.Duration, err error) {
	ctx := r.Context()
	f := r.PostForm
	current, answer := f.Get("current"), f.Get("confirm_passkey")
	byPasskey := current == "" && answer != "" && s.d.Passkeys != nil
	if current == "" && !byPasskey {
		return confirmMissing, 0, nil
	}
	attempt, wait := ratelimit.BeginPair(ctx, s.passwordDelay, v.user.Handle, s.addressDelay, ratelimit.IPKey(r))
	if wait > 0 {
		return confirmWait, wait, nil
	}
	if byPasskey {
		err = s.d.Passkeys.Confirm(ctx, v.user, f.Get("confirm_token"), answer)
	} else {
		err = s.d.Users.ConfirmPassword(ctx, v.user.Handle, current)
	}
	switch {
	case errors.Is(err, user.ErrBadPassword):
		attempt.Failed()
		return confirmWrong, 0, nil
	case errors.Is(err, passkey.ErrRejected):
		attempt.Failed()
		return confirmPasskey, 0, nil
	case errors.Is(err, passkey.ErrExpired):
		attempt.Done()
		return confirmExpired, 0, nil
	}
	attempt.Done()
	if err != nil {
		return "", 0, err
	}
	s.passwordDelay.Reset(v.user.Handle)
	return "", 0, nil
}

// refused answers a refused confirmation of v's form with the account page
// again, the reason under the form's field errKey, and reports whether it
// did. A failure that is nobody's doing gets its own page.
func (s *server) refused(w http.ResponseWriter, r *http.Request, v visit, errKey string, d accountPageData) bool {
	why, wait, err := s.confirm(r, v)
	switch {
	case errors.Is(err, user.ErrBusy):
		s.busy(w, r)
		return true
	case err != nil:
		s.fail(w, r, "confirm", err)
		return true
	case why == "":
		return false
	}
	status := http.StatusUnprocessableEntity
	switch why {
	case confirmWait:
		w.Header().Set("Retry-After", ratelimit.RetryAfter(wait))
		d.Seconds = max(1, int((wait+time.Second-1)/time.Second))
		status = http.StatusTooManyRequests
	case confirmExpired:
		status = http.StatusBadRequest
	}
	if d.Err == nil {
		d.Err = map[string]string{}
	}
	d.Err[errKey] = why
	s.renderAccount(w, r, status, v, d)
	return true
}

// confirmField is the field of a form that confirms it, as the template
// "confirm-field" draws it.
type confirmField struct {
	ID      string
	Why     string
	Seconds int
	// Undone says what did not happen, after a refusal.
	Undone string
	Help   string
	Wide   bool
	// Passkey offers the person's passkeys in place of the password; nil
	// when they have none.
	Passkey *passkey.Ceremony
}

// ConfirmField returns the confirmation field with id, whose refusal is
// Err[errKey].
func (d accountPageData) ConfirmField(id, errKey, undone, help string, wide bool) confirmField {
	return confirmField{ID: id, Why: d.Err[errKey], Seconds: d.Seconds, Undone: undone, Help: help, Wide: wide, Passkey: d.Confirm}
}
