package gate_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/user"
)

// post sends the password form for p from addr, with token in the share
// cookie ("" for none) and the page's own origin.
func (fx *fixture) post(p *artifact.Published, token, addr, password string) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, gate.SharePath, strings.NewReader(form.Encode()))
	r.Host = "team.art.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://team.art.test")
	r.RemoteAddr = addr + ":4321"
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-kazari_share", Value: token})
	}
	return fx.reserved(r, p, "team")
}

// unlock posts a password for lockedToken from one address.
func (fx *fixture) unlock(password string) *httptest.ResponseRecorder {
	return fx.post(team, lockedToken, "198.51.100.7", password)
}

// A link with a password moves its token off the address bar into a short
// cookie, shows a form, and the right password gives the cookie the page
// then takes.
func TestSharePassword(t *testing.T) {
	fx := newFixture()
	rec := fx.reserved(request(http.MethodGet, gate.SharePath+"?token="+lockedToken, navigate), team, "team")
	share := cookie(rec, "__Host-kazari_share")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != gate.SharePath || share == nil ||
		share.Value != lockedToken || !share.HttpOnly || !share.Secure || share.MaxAge != int((10*time.Minute)/time.Second) {
		t.Fatalf("opening a link with a password: status %d, Location %q, share cookie %+v; "+
			"want 303 to the bare share path and the token in a ten-minute cookie", rec.Code, rec.Header().Get("Location"), share)
	}
	if c := cookie(rec, "__Host-kazari_view"); c != nil || fx.links.opened != 0 {
		t.Errorf("opening the link set the view cookie %+v or counted a use (%d)", c, fx.links.opened)
	}

	rec = fx.reserved(request(http.MethodGet, gate.SharePath, navigate, share), team, "team")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `name="password"`) || strings.Contains(body, lockedToken) ||
		!strings.Contains(body, `autocomplete="current-password"`) {
		t.Fatalf("the password form: status %d, want 200 and a form that does not hold the token:\n%s", rec.Code, body)
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
	if c := cookie(rec, "__Host-kazari_share"); c == nil || c.MaxAge >= 0 {
		t.Errorf("right password left the share cookie: %+v", c)
	}
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, view), team, "team"); !ok {
		t.Error("the unlocked cookie did not open the page")
	}

	calls := len(fx.links.unlocked)
	r := httptest.NewRequest(http.MethodPost, gate.SharePath, strings.NewReader("password="+url.QueryEscape(lockedPassword)))
	r.Host = "team.art.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.art.test")
	r.AddCookie(&http.Cookie{Name: "__Host-kazari_share", Value: lockedToken})
	if rec := fx.reserved(r, team, "team"); rec.Code != http.StatusForbidden || len(fx.links.unlocked) != calls {
		t.Errorf("password posted from another host: status %d, unlock calls %d; want 403 and no check",
			rec.Code, len(fx.links.unlocked)-calls)
	}
	if rec := fx.unlock(""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Enter the password.") {
		t.Errorf("empty password: status %d, want 400 asking for it", rec.Code)
	}
	if rec := fx.post(team, "", "198.51.100.7", lockedPassword); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "has expired") || len(fx.links.unlocked) != calls {
		t.Errorf("password without the share cookie: status %d, want 403 saying the form expired, and no check", rec.Code)
	}
}

// One address's wrong guesses slow that address down, and hold up the
// link's other visitors only once they are many.
func TestSharePasswordGuessing(t *testing.T) {
	fx := newFixture()
	for _, guess := range []string{"guess one", "guess two", "guess three", "guess four"} {
		if rec := fx.unlock(guess); rec.Code != http.StatusForbidden {
			t.Fatalf("guess %q: status %d, want 403", guess, rec.Code)
		}
	}
	rec := fx.unlock(lockedPassword)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "Too many tries") {
		t.Errorf("a guess too soon: status %d, Retry-After %q; want 429 with a wait", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := len(fx.links.unlocked); got != 4 {
		t.Errorf("passwords checked = %d, want 4: the early one is refused before it is checked", got)
	}
	// The recipient, elsewhere, is not held up by that.
	if rec := fx.post(team, lockedToken, "203.0.113.9", lockedPassword); rec.Code != http.StatusFound {
		t.Errorf("the right password from another address: status %d, want 302", rec.Code)
	}
}

// Guesses spread over many addresses meet the link's own limit.
func TestSharePasswordGuessingAcrossAddresses(t *testing.T) {
	fx := newFixture()
	failures := 0
	for i := 1; failures < 31; i++ {
		for range 3 {
			if failures == 31 {
				break
			}
			addr := fmt.Sprintf("198.51.100.%d", i)
			if rec := fx.post(team, lockedToken, addr, "a wrong guess"); rec.Code != http.StatusForbidden {
				t.Fatalf("wrong guess %d from %s: status %d, want 403", failures+1, addr, rec.Code)
			}
			failures++
		}
	}
	rec := fx.post(team, lockedToken, "203.0.113.50", lockedPassword)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("after %d wrong guesses from many addresses, a fresh address: status %d, want 429", failures, rec.Code)
	}
}

// A token that opens nothing is not a wrong password: it does not count.
func TestSharePasswordUnknownTokenDoesNotCount(t *testing.T) {
	fx := newFixture()
	unknown := artifact.LinkTokenPrefix + "uuuuuuuu_" + strings.Repeat("v", 52)
	for i := range 10 {
		if rec := fx.post(team, unknown, "198.51.100.7", "anything at all"); rec.Code != http.StatusForbidden ||
			!strings.Contains(rec.Body.String(), "Link not valid") {
			t.Fatalf("try %d with an unknown token: status %d, want 403 Link not valid", i+1, rec.Code)
		}
	}
	if got := len(fx.links.unlocked); got != 10 {
		t.Errorf("unknown-token tries checked = %d, want all 10: none counted as a failure", got)
	}
}

func TestSharePasswordBusy(t *testing.T) {
	fx := newFixture()
	fx.links.err = user.ErrBusy
	rec := fx.unlock(lockedPassword)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "busy") {
		t.Errorf("busy hashing: status %d, Retry-After %q; want 503 asking to try again", rec.Code, rec.Header().Get("Retry-After"))
	}
	fx.links.err = context.Canceled
	if rec := fx.unlock(lockedPassword); rec.Body.Len() != 0 {
		t.Errorf("a visitor who left: answered %d %q, want nothing written", rec.Code, rec.Body.String())
	}
}

// A staged version opens for no share link, password or not.
func TestSharePasswordStaged(t *testing.T) {
	fx := newFixture()
	staged := &artifact.Published{Slug: "team", VersionID: 3, VersionNumber: 2, Visibility: artifact.Members, Staged: true}
	if rec := fx.post(staged, lockedToken, "198.51.100.7", lockedPassword); rec.Code != http.StatusForbidden ||
		len(fx.links.unlocked) != 0 {
		t.Errorf("unlocking on a staged version: status %d, unlock calls %d; want 403 and no check", rec.Code, len(fx.links.unlocked))
	}
	view := &http.Cookie{Name: "__Host-kazari_view", Value: lockedCookie}
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, view), staged, "team--v2"); ok {
		t.Error("a password link's cookie opened a staged version")
	}
}
