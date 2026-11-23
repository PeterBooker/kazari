package user_test

import (
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/user"
)

// HashPassword and VerifyPassword serve passwords that guard something
// other than an account, such as a share link.
func TestHashAndVerifyPassword(t *testing.T) {
	f := newFixture(t, user.Options{Argon2: user.Argon2{Memory: 64, Time: 1, Threads: 1}})
	hash, err := f.svc.HashPassword(t.Context(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, "correct horse") {
		t.Fatalf("HashPassword = %q, want an argon2id hash that does not hold the password", hash)
	}
	for _, tc := range []struct {
		name, hash, password string
		want                 bool
	}{
		{"the right password", hash, "correct horse battery", true},
		{"a wrong password", hash, "correct horse battery!", false},
		{"no hash", "", "correct horse battery", false},
	} {
		got, err := f.svc.VerifyPassword(t.Context(), tc.hash, tc.password)
		if err != nil || got != tc.want {
			t.Errorf("%s: VerifyPassword = %t, %v; want %t", tc.name, got, err, tc.want)
		}
	}
	if _, err := f.svc.VerifyPassword(t.Context(), "$argon2id$not a hash", "x"); err == nil {
		t.Error("VerifyPassword of a hash it cannot read: err = nil, want an error")
	}
}
