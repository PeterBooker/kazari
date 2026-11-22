package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/user"
)

// linkPageData is the form a link opens.
type linkPageData struct {
	base
	Purpose   string
	For       *user.User // nil for setup
	MinLength int
	MaxLength int
	// What the person typed for the first admin, kept across a refusal.
	NewHandle, Name, Email string
	// Password is kept across a refusal of another field alone, so that
	// only what was wrong is typed again. Suggestion is a handle that
	// would do, made from what was typed.
	Password   string
	Suggestion string
	// Err names what is wrong with a field: the template has the words.
	Err map[string]string
}

func (s *server) linkCookieName() string {
	if s.o.InsecureCookies {
		return linkCookie
	}
	return "__Host-" + linkCookie
}

// setLinkCookie holds the link's token while its form is open. The __Host-
// prefix keeps script on an artifact host from planting another person's
// link here.
func (s *server) setLinkCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{
		Name:     s.linkCookieName(),
		Value:    token,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   int(linkCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   !s.o.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// linkToken returns the token of the open link, or "".
func (s *server) linkToken(r *http.Request) string {
	ck, err := r.Cookie(s.linkCookieName())
	if err != nil || !user.IsLinkToken(ck.Value) {
		return ""
	}
	return ck.Value
}

// linkFormToken ties a link's form to the page that link opened. The cookie
// goes with every request a browser makes to this host, whoever wrote the
// form; only a page that was rendered for the link holds this.
func linkFormToken(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("kazari account link form"))
	return hex.EncodeToString(mac.Sum(nil))
}

// openLink loads the live link of the request's cookie for purpose. When ok
// is false it has answered: the link is not valid, whatever the reason, or
// the address has to wait.
func (s *server) openLink(w http.ResponseWriter, r *http.Request, purpose user.Purpose) (string, *user.User, bool) {
	attempt, d := s.linkDelay.Begin(ratelimit.IPKey(r))
	if d > 0 {
		s.linkDelay.Refused(r.Context())
		s.wait(w, r, d)
		return "", nil, false
	}
	token := s.linkToken(r)
	if token == "" {
		attempt.Done()
		s.message(w, r, http.StatusNotFound, "link-invalid")
		return "", nil, false
	}
	l, u, err := s.d.Users.Link(r.Context(), token)
	if errors.Is(err, user.ErrLinkNotFound) || errors.Is(err, user.ErrSetupDone) {
		s.linkFailed(w, r, attempt)
		return "", nil, false
	}
	attempt.Done()
	if err != nil {
		s.fail(w, r, "load link", err)
		return "", nil, false
	}
	if l.Purpose != purpose {
		s.message(w, r, http.StatusNotFound, "link-invalid")
		return "", nil, false
	}
	return token, u, true
}

// linkFailed answers for a token that opens nothing, and counts it against
// the address. The cookie goes, so reloading the page does not count again.
func (s *server) linkFailed(w http.ResponseWriter, r *http.Request, attempt ratelimit.Attempt) {
	attempt.Failed()
	s.setLinkCookie(w, "")
	s.message(w, r, http.StatusNotFound, "link-invalid")
}

// linkPage opens a link. With the token in the address it only moves it
// into the cookie and redirects to the clean address: the token then is in
// no address bar, history entry or Referer, and nothing was looked up or
// used, so a chat application that fetches the link for a preview changes
// nothing. Without one it shows the form.
func (s *server) linkPage(purpose user.Purpose) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Has("token") {
			token := q.Get("token")
			if !user.IsLinkToken(token) {
				s.message(w, r, http.StatusNotFound, "link-invalid")
				return
			}
			s.setLinkCookie(w, token)
			http.Redirect(w, r, Prefix+string(purpose), http.StatusSeeOther)
			return
		}
		token, u, ok := s.openLink(w, r, purpose)
		if !ok {
			return
		}
		s.renderLink(w, r, http.StatusOK, purpose, token, u, linkPageData{})
	}
}

func (s *server) renderLink(w http.ResponseWriter, r *http.Request, status int, purpose user.Purpose, token string, u *user.User, d linkPageData) {
	d.base = s.base()
	d.FormToken = linkFormToken(token)
	d.Purpose = string(purpose)
	d.For = u
	d.MinLength = s.d.Users.MinPasswordLength()
	d.MaxLength = user.MaxPasswordLength
	// Browsers hold a form's submission to form-action through every
	// redirect that follows it, and this form's ends on the sign-in host.
	w.Header().Set("Content-Security-Policy", s.linkCSP)
	s.render(w, r, status, "link", d)
}

// redeemLink uses the open link up: it sets the password, and for a setup
// link creates the first admin. The person is then signed in here, as the
// link and the new password show who they are, and lands on the account
// page's welcome. Every field is checked before anything is used, so one
// answer names every mistake.
func (s *server) redeemLink(purpose user.Purpose) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			s.message(w, r, http.StatusBadRequest, "bad-form")
			return
		}
		f := r.PostForm
		// Before the link is looked up or anything is counted. Without a
		// cookie there is no link, which openLink answers.
		if open := s.linkToken(r); open != "" && !hmac.Equal([]byte(f.Get(formTokenField)), []byte(linkFormToken(open))) {
			s.message(w, r, http.StatusForbidden, "link-forbidden")
			return
		}
		token, u, ok := s.openLink(w, r, purpose)
		if !ok {
			return
		}
		d := linkPageData{NewHandle: strings.TrimSpace(f.Get("handle")), Name: strings.TrimSpace(f.Get("name")), Email: strings.TrimSpace(f.Get("email"))}
		password := f.Get("password")
		errs := map[string]string{}
		if purpose == user.PurposeSetup && user.ValidateHandle(d.NewHandle) != nil {
			errs["handle"] = "invalid"
		}
		if n := utf8.RuneCountInString(password); n < s.d.Users.MinPasswordLength() || n > user.MaxPasswordLength {
			errs["password"] = "weak"
		} else if password != f.Get("confirm") {
			errs["confirm"] = "mismatch"
		}
		refuse := func(errs map[string]string) {
			d.Err = errs
			if errs["handle"] != "" {
				d.Suggestion = user.SuggestHandle(d.NewHandle)
				if d.Suggestion == "" {
					d.Suggestion = user.SuggestHandle(d.Name)
				}
			}
			if errs["password"] == "" && errs["confirm"] == "" {
				d.Password = password
			}
			s.renderLink(w, r, http.StatusUnprocessableEntity, purpose, token, u, d)
		}
		if len(errs) > 0 {
			refuse(errs)
			return
		}
		redeemed, err := s.d.Users.Redeem(r.Context(), token, user.RedeemRequest{
			Password: password, Handle: d.NewHandle, Email: d.Email, DisplayName: d.Name,
		})
		switch {
		case errors.Is(err, user.ErrWeakPassword):
			refuse(map[string]string{"password": "weak"})
			return
		case errors.Is(err, user.ErrInvalidHandle):
			refuse(map[string]string{"handle": "invalid"})
			return
		case errors.Is(err, user.ErrBusy):
			s.busy(w, r)
			return
		case errors.Is(err, user.ErrLinkNotFound), errors.Is(err, user.ErrSetupDone):
			// Lost a race for the link since it was opened above.
			attempt, _ := s.linkDelay.Begin(ratelimit.IPKey(r))
			s.linkFailed(w, r, attempt)
			return
		case err != nil:
			s.fail(w, r, "redeem link", err)
			return
		}
		s.setLinkCookie(w, "")
		// The password is set whatever follows: without a session the next
		// page asks for it, as it always did.
		if err := s.sess.Start(r.Context(), w, redeemed.Handle); err != nil {
			s.log.ErrorContext(r.Context(), "account: sign in after link", "err", err)
			s.done(w, r, "password-set")
			return
		}
		s.done(w, r, "welcome")
	}
}
