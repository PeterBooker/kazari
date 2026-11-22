package gate_test

import (
	"net/http"
	"testing"
)

// The right password forgets the wrong ones before it, so a recipient who
// mistyped starts afresh next time.
func TestSharePasswordSuccessResets(t *testing.T) {
	fx := newFixture()
	for _, guess := range []string{"typo one", "typo two", "typo three"} {
		if rec := fx.unlock(guess); rec.Code != http.StatusForbidden {
			t.Fatalf("typo %q: status %d, want 403", guess, rec.Code)
		}
	}
	if rec := fx.unlock(lockedPassword); rec.Code != http.StatusFound {
		t.Fatalf("the right password after three typos: status %d, want 302", rec.Code)
	}
	for _, guess := range []string{"typo four", "typo five", "typo six", "typo seven"} {
		if rec := fx.unlock(guess); rec.Code != http.StatusForbidden {
			t.Errorf("typo %q after a success: status %d, want 403: the earlier failures should be forgotten", guess, rec.Code)
		}
	}
}
