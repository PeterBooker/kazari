package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/user"
)

// A caller turned away because too many passwords are being hashed is told
// when to come back, and is not logged as a fault.
func TestBusyAnswersRetryAfter(t *testing.T) {
	var logged strings.Builder
	b := newBase(slog.New(slog.NewTextHandler(&logged, nil)), present.Links{}, instrument.Providers{})
	w := httptest.NewRecorder()
	b.writeError(w, httptest.NewRequest(http.MethodPost, "/v1/invites", nil), fmt.Errorf("user: redeem: %w", user.ErrBusy))

	var body api.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable || body.Error.Code != api.CodeBusy || w.Header().Get("Retry-After") != "5" {
		t.Errorf("status %d, code %q, Retry-After %q; want 503, busy, 5", w.Code, body.Error.Code, w.Header().Get("Retry-After"))
	}
	if logged.Len() != 0 {
		t.Errorf("a busy answer was logged: %s", logged.String())
	}
}
