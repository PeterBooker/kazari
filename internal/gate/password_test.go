package gate_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/gate"
)

// unlock posts a password for lockedToken from the page itself.
func (fx *fixture) unlock(password string) *httptest.ResponseRecorder {
	return fx.unlockFrom("https://team.art.test", password)
}

func (fx *fixture) unlockFrom(origin, password string) *httptest.ResponseRecorder {
	form := url.Values{"token": {lockedToken}, "password": {password}}
	r := httptest.NewRequest(http.MethodPost, gate.SharePath, strings.NewReader(form.Encode()))
	r.Host = "team.art.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	r.RemoteAddr = "198.51.100.7:4321"
	return fx.reserved(r, team, "team")
}

// A link with a password opens a form, not the page, and the right password
// gives the cookie the page then takes.
func TestSharePassword(t *testing.T) {
	fx := newFixture()
	rec := fx.reserved(request(http.MethodGet, gate.SharePath+"?token="+lockedToken, navigate), team, "team")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `name="password"`) || !strings.Contains(body, `value="`+lockedToken+`"`) ||
		!strings.Contains(body, `action="/.kazari/share"`) {
		t.Fatalf("opening a link with a password: status %d, want 200 and the password form:\n%s", rec.Code, body)
	}
	if c := cookie(rec, "__Host-kazari_view"); c != nil || fx.links.opened != 0 {
		t.Errorf("the form set the view cookie %+v or counted a use (%d)", c, fx.links.opened)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") || strings.Contains(got, "script-src") {
		t.Errorf("password page CSP = %q, want the gate's own, without script", got)
	}

	rec = fx.unlock("open says me")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "That password is not right.") ||
		cookie(rec, "__Host-kazari_view") != nil {
		t.Errorf("wrong password: status %d, want 403, the form again and no cookie:\n%s", rec.Code, rec.Body.String())
	}
	rec = fx.unlock(lockedPassword)
	view := cookie(rec, "__Host-kazari_view")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" || view == nil || view.Value != lockedCookie ||
		!view.HttpOnly || view.MaxAge != int((24*time.Hour)/time.Second) {
		t.Fatalf("right password: status %d, Location %q, cookie %+v; want 302 to / and the link's cookie",
			rec.Code, rec.Header().Get("Location"), view)
	}
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, view), team, "team"); !ok {
		t.Error("the unlocked cookie did not open the page")
	}

	calls := len(fx.links.unlocked)
	if rec := fx.unlockFrom("https://evil.art.test", lockedPassword); rec.Code != http.StatusForbidden || len(fx.links.unlocked) != calls {
		t.Errorf("password posted from another host: status %d, unlock calls %d; want 403 and no check",
			rec.Code, len(fx.links.unlocked)-calls)
	}
	if rec := fx.unlock(""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Enter the password.") {
		t.Errorf("empty password: status %d, want 400 asking for it", rec.Code)
	}
}

// Guesses at a link's password slow down: a few are free, then each waits,
// and a guess that comes too early is not even checked.
func TestSharePasswordGuessing(t *testing.T) {
	fx := newFixture()
	for _, guess := range []string{"guess one", "guess two", "guess three", "guess four"} {
		if rec := fx.unlock(guess); rec.Code != http.StatusForbidden {
			t.Fatalf("guess %q: status %d, want 403", guess, rec.Code)
		}
	}
	rec := fx.unlock(lockedPassword)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "Too many tries") {
		t.Errorf("a guess too soon: status %d, Retry-After %q; want 429 with a wait", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := len(fx.links.unlocked); got != 4 {
		t.Errorf("passwords checked = %d, want 4: the early one is refused before it is checked", got)
	}
}
