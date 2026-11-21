package account

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/theme"
)

// A password the server was too busy to check or set is answered with a
// page that says so and when to try again, not with the page for a fault.
func TestBusyPage(t *testing.T) {
	s := &server{
		log:    slog.Default(),
		tmpl:   template.Must(theme.Templates().Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		assets: theme.URLs(assetPrefix),
		host:   "art.test",
	}
	w := httptest.NewRecorder()
	s.busy(w, httptest.NewRequest(http.MethodPost, "https://art.test/account/password", nil))
	body := w.Body.String()
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if !strings.Contains(body, "Nothing was changed") || !strings.Contains(body, "in a moment") || strings.Contains(body, "Something went wrong") {
		t.Errorf("the busy page: %s", body)
	}
}
