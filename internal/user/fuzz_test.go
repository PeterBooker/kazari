package user_test

import (
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/user"
)

func FuzzParseKey(f *testing.F) {
	prefix, secret := strings.Repeat("a", user.PrefixLen), strings.Repeat("b", user.SecretLen)
	for _, s := range []string{
		user.KeyPrefix + prefix + "_" + secret,
		user.KeyPrefix + "abcdefg2" + "_" + strings.Repeat("7z", user.SecretLen/2),
		user.KeyPrefix + prefix + "_" + secret + "b",
		user.KeyPrefix + prefix[1:] + "_" + secret,
		user.KeyPrefix + prefix + secret,
		user.KeyPrefix + prefix + "_" + strings.Repeat("B", user.SecretLen),
		user.KeyPrefix + prefix + "_" + strings.Repeat("1", user.SecretLen),
		user.KeyPrefix + prefix + "__" + secret[1:],
		"kzl_" + prefix + "_" + secret,
		prefix + "_" + secret, user.KeyPrefix, "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		prefix, secret, ok := user.ParseKey(token)
		if !ok {
			if prefix != "" || secret != "" {
				t.Errorf("ParseKey(%q) = (%q, %q, false), want (\"\", \"\", false)", token, prefix, secret)
			}
			return
		}
		if len(token) != user.KeyLen || token != user.KeyPrefix+prefix+"_"+secret {
			t.Errorf("ParseKey(%q) = (%q, %q, true), want the parts of the token", token, prefix, secret)
		}
		const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
		if len(prefix) != user.PrefixLen || len(secret) != user.SecretLen || strings.Trim(prefix+secret, alphabet) != "" {
			t.Errorf("ParseKey(%q) = (%q, %q, true), want %d and %d characters of base32", token, prefix, secret, user.PrefixLen, user.SecretLen)
		}
	})
}

func FuzzSuggestHandle(f *testing.F) {
	for _, s := range []string{"Ana Admin", "ana", "admin", "  Zoë Ø'Brien ", "_x", "a", strings.Repeat("Long Name ", 8), "", "--", "ana--b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := user.SuggestHandle(s)
		if got != "" && user.ValidateHandle(got) != nil {
			t.Errorf("SuggestHandle(%q) = %q, which is not a valid handle", s, got)
		}
		if user.ValidateHandle(s) == nil && got != s {
			t.Errorf("SuggestHandle(%q) = %q, want the valid handle itself", s, got)
		}
		if again := user.SuggestHandle(got); got != "" && again != got {
			t.Errorf("SuggestHandle(%q) = %q, want it unchanged", got, again)
		}
	})
}

func TestSuggestHandle(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"a name", "Ana Admin", "ana-admin"},
		{"a valid handle", "ana_b", "ana_b"},
		{"punctuation", "  Ana O'Brien! ", "ana-o-brien"},
		{"letters it cannot keep", "Zoë", "zo"},
		{"reserved", "Admin", ""},
		{"too short", "A", ""},
		{"too long", strings.Repeat("ab ", 20), "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := user.SuggestHandle(tt.in); got != tt.want {
				t.Errorf("SuggestHandle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
