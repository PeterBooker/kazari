package session

import (
	"context"
	"errors"
	"net/http"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
)

// FormField is the form field that carries the session's form token.
const FormField = "form_token"

// maxFormBytes bounds a posted form; none of a control host carries a file.
const maxFormBytes = 64 << 10

// The refusals of Require and CheckForm. Each is counted in the session
// authentications before it is returned; the host writes the page.
var (
	// ErrSignInStarted: the browser was sent to sign in, and the response
	// is written.
	ErrSignInStarted = errors.New("session: sent to sign in")
	// ErrSignedOut: no live session, on a request that is not a page load.
	ErrSignedOut = errors.New("session: not signed in")
	// ErrBadForm: the form could not be read.
	ErrBadForm = errors.New("session: form not readable")
	// ErrFormToken: the form does not carry the session's form token.
	ErrFormToken = errors.New("session: form token not valid")
	// ErrRefused: the host turned the signed-in person away.
	ErrRefused = errors.New("session: refused")
)

// Require returns the session of the request. Without one, a page load is
// sent to sign in and anything else is refused, with ErrSignInStarted or
// ErrSignedOut. What the person may do here is still the caller's to check.
func (m *Manager) Require(w http.ResponseWriter, r *http.Request) (*Session, error) {
	sess, err := m.Get(r)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, m.SignedOut(w, r)
	}
	return sess, nil
}

// Guard admits a request to a page of the host: it returns the request's
// session, and the request carrying the principal admit says the person acts
// as, counted as let through. Without a session a page load is sent to sign
// in and anything else is refused (ErrSignInStarted, ErrSignedOut). A person
// admit turns away is counted and refused with ErrRefused, the session still
// returned for the host's page. With form set, the posted form must carry
// the session's token (ErrBadForm, ErrFormToken).
func (m *Manager) Guard(w http.ResponseWriter, r *http.Request, form bool, admit func(*Session) (auth.Principal, bool)) (*Session, *http.Request, error) {
	sess, err := m.Require(w, r)
	if err != nil {
		return nil, r, err
	}
	p, ok := admit(sess)
	if !ok {
		m.Refuse(r.Context())
		return sess, r, ErrRefused
	}
	if form {
		if err := sess.CheckForm(w, r); err != nil {
			return sess, r, err
		}
	}
	m.Admit(r.Context(), p)
	return sess, r.WithContext(auth.ContextWithPrincipal(r.Context(), p)), nil
}

// SignedOut answers a request whose session is gone, or names a person who
// is: a page load is sent to sign in again (ErrSignInStarted), anything else
// gets ErrSignedOut.
func (m *Manager) SignedOut(w http.ResponseWriter, r *http.Request) error {
	result := auth.ResultMissing
	if m.Presented(r) {
		result = auth.ResultInvalid
	}
	m.auth.Record(r.Context(), result, nil)
	if middleware.Navigation(r) {
		m.SignIn(w, r, r.URL.RequestURI())
		return ErrSignInStarted
	}
	return ErrSignedOut
}

// CheckForm reads the posted form, bounded in size, and checks that it
// carries the session's form token: ErrBadForm or ErrFormToken.
func (s *Session) CheckForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return ErrBadForm
	}
	if !s.ValidForm(r.PostForm.Get(FormField)) {
		s.m.auth.Record(r.Context(), auth.ResultForbidden, nil)
		return ErrFormToken
	}
	return nil
}

// Admit counts a request the host lets through, as p.
func (m *Manager) Admit(ctx context.Context, p auth.Principal) {
	m.auth.Record(ctx, auth.ResultOK, &p)
}

// Refuse counts a request of a signed-in person the host turns away.
func (m *Manager) Refuse(ctx context.Context) {
	m.auth.Record(ctx, auth.ResultForbidden, nil)
}

// Callback ends a sign-in: the code becomes the session cookie and the
// browser is redirected to the path it asked for. On an error, ErrSignIn
// among them, nothing is written but the cookie.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) (*Session, error) {
	sess, path, err := m.Open(w, r)
	if err != nil {
		return nil, err
	}
	// Location is set as is: http.Redirect would clean the path, and
	// cleaning can turn a path on this host into one a browser reads as
	// another host's.
	w.Header().Set("Location", path)
	w.WriteHeader(http.StatusFound)
	return sess, nil
}

// SignOut ends the sign-in on every host. A browser with a live session
// must send its form token (ErrBadForm, ErrFormToken); one whose session
// already ended is signed out all the same. The host writes the page.
func (m *Manager) SignOut(w http.ResponseWriter, r *http.Request) error {
	sess, err := m.Get(r)
	if err != nil {
		return err
	}
	if sess != nil {
		if err := sess.CheckForm(w, r); err != nil {
			if errors.Is(err, ErrBadForm) {
				m.Refuse(r.Context())
			}
			return err
		}
	}
	return m.End(w, r)
}
