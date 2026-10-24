package user

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
)

// Key format: agk_<prefix>_<secret>, both lower-case unpadded base32.
const (
	prefixBytes = 5  // 40 bits encode to exactly 8 characters
	secretBytes = 32 // 256 bits encode to 52 characters

	// PrefixLen is the length of a key's lookup prefix.
	PrefixLen = 8
	// SecretLen is the length of a key's secret part.
	SecretLen = 52
	// KeyLen is the length of a whole key string.
	KeyLen = len(KeyPrefix) + PrefixLen + 1 + SecretLen
)

var keyEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newKey returns a fresh key string and its prefix.
func newKey() (key, prefix string, err error) {
	var b [prefixBytes + secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("user: generate key: %w", err)
	}
	prefix = keyEncoding.EncodeToString(b[:prefixBytes])
	secret := keyEncoding.EncodeToString(b[prefixBytes:])
	return KeyPrefix + prefix + "_" + secret, prefix, nil
}

// ParseKey splits a key string into its prefix and secret. ok is false
// unless token has the exact agk_<prefix>_<secret> shape.
func ParseKey(token string) (prefix, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, KeyPrefix)
	if !found {
		return "", "", false
	}
	prefix, secret, found = strings.Cut(rest, "_")
	if !found || len(prefix) != PrefixLen || len(secret) != SecretLen ||
		!isBase32(prefix) || !isBase32(secret) {
		return "", "", false
	}
	return prefix, secret, true
}

func isBase32(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// HashKey returns the hex SHA-256 of the whole key string, as stored in
// APIKey.Hash.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
