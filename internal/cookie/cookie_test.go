package cookie_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/cookie"
)

func TestSet(t *testing.T) {
	tests := []struct {
		name     string
		insecure bool
		ttl      time.Duration
		wantName string
		wantAge  int
	}{
		{"secure", false, time.Hour, "__Host-kazari_test", 3600},
		{"insecure", true, time.Hour, "kazari_test", 3600},
		{"whole seconds", false, 1500 * time.Millisecond, "__Host-kazari_test", 1},
		{"under a second deletes", false, 999 * time.Millisecond, "__Host-kazari_test", -1},
		{"zero deletes", false, 0, "__Host-kazari_test", -1},
		{"negative deletes", false, -time.Hour, "__Host-kazari_test", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			cookie.Host{Insecure: tt.insecure}.Set(rec, "kazari_test", "v", tt.ttl)
			cks := rec.Result().Cookies()
			if len(cks) != 1 {
				t.Fatalf("Set wrote %d cookies, want 1", len(cks))
			}
			ck := cks[0]
			if ck.Name != tt.wantName || ck.MaxAge != tt.wantAge {
				t.Errorf("Set(%v) = %s with MaxAge %d, want %s with MaxAge %d", tt.ttl, ck.Name, ck.MaxAge, tt.wantName, tt.wantAge)
			}
			if ck.Path != "/" || ck.Domain != "" || !ck.HttpOnly || ck.Secure == tt.insecure || ck.SameSite != http.SameSiteLaxMode {
				t.Errorf("Set: Path %q, Domain %q, HttpOnly %v, Secure %v, SameSite %v; want /, none, true, %v, Lax",
					ck.Path, ck.Domain, ck.HttpOnly, ck.Secure, ck.SameSite, !tt.insecure)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	rec := httptest.NewRecorder()
	cookie.Host{}.Delete(rec, "kazari_test")
	cks := rec.Result().Cookies()
	if len(cks) != 1 || cks[0].Name != "__Host-kazari_test" || cks[0].MaxAge != -1 || cks[0].Value != "" {
		t.Errorf("Delete wrote %+v, want __Host-kazari_test empty with MaxAge -1", cks)
	}
}

func TestGet(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-kazari_test", Value: "secure"})
	r.AddCookie(&http.Cookie{Name: "kazari_test", Value: "plain"})
	for _, tt := range []struct {
		h    cookie.Host
		base string
		want string
	}{
		{cookie.Host{}, "kazari_test", "secure"},
		{cookie.Host{Insecure: true}, "kazari_test", "plain"},
		{cookie.Host{}, "kazari_other", ""},
	} {
		if got := tt.h.Get(r, tt.base); got != tt.want {
			t.Errorf("Host{Insecure: %v}.Get(%q) = %q, want %q", tt.h.Insecure, tt.base, got, tt.want)
		}
	}
}
