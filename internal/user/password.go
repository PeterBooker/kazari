package user

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2 holds the argon2id parameters of a password hash.
type Argon2 struct {
	Memory  uint32 // KiB
	Time    uint32 // passes
	Threads uint8
}

// DefaultArgon2 is the second recommended option of RFC 9106: 64 MiB,
// three passes, four lanes.
var DefaultArgon2 = Argon2{Memory: 64 << 10, Time: 3, Threads: 4}

const (
	saltLen = 16
	hashLen = 32

	// maxArgon2Memory bounds what a stored hash can make a sign-in allocate.
	maxArgon2Memory = 1 << 20 // KiB
	maxArgon2Time   = 16

	// MaxPasswordLength is the longest password accepted, in characters.
	MaxPasswordLength = 256
	// maxPasswordBytes is that many four-byte characters.
	maxPasswordBytes = 4 * MaxPasswordLength
)

var b64 = base64.RawStdEncoding

// hashPassword returns the PHC string of password under p, with a new salt.
func hashPassword(p Argon2, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("user: hash password: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, hashLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the PHC string phc, and
// the parameters phc was made with. A string it cannot read is an error.
func verifyPassword(phc, password string) (bool, Argon2, error) {
	p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, Argon2{}, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, p, nil
}

var errBadHash = errors.New("user: stored password hash is not a valid argon2id string")

func parsePHC(phc string) (Argon2, []byte, []byte, error) {
	// "$argon2id$v=19$m=65536,t=3,p=4$salt$key" splits into six fields, the
	// first empty.
	f := strings.Split(phc, "$")
	if len(f) != 6 || f[0] != "" || f[1] != "argon2id" || f[2] != "v="+strconv.Itoa(argon2.Version) {
		return Argon2{}, nil, nil, errBadHash
	}
	var p Argon2
	for kv := range strings.SplitSeq(f[3], ",") {
		k, v, _ := strings.Cut(kv, "=")
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Argon2{}, nil, nil, errBadHash
		}
		switch k {
		case "m":
			p.Memory = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > 255 {
				return Argon2{}, nil, nil, errBadHash
			}
			p.Threads = uint8(n)
		default:
			return Argon2{}, nil, nil, errBadHash
		}
	}
	if p.Memory == 0 || p.Memory > maxArgon2Memory || p.Time == 0 || p.Time > maxArgon2Time || p.Threads == 0 {
		return Argon2{}, nil, nil, errBadHash
	}
	salt, err := b64.DecodeString(f[4])
	if err != nil || len(salt) < 8 {
		return Argon2{}, nil, nil, errBadHash
	}
	key, err := b64.DecodeString(f[5])
	if err != nil || len(key) < 16 {
		return Argon2{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// checkPolicy reports ErrWeakPassword unless password is min to
// MaxPasswordLength characters of valid UTF-8. Length is the only rule.
func checkPolicy(password string, min int) error {
	if !utf8.ValidString(password) || len(password) > maxPasswordBytes {
		return fmt.Errorf("%w: use at most %d characters", ErrWeakPassword, MaxPasswordLength)
	}
	switch n := utf8.RuneCountInString(password); {
	case n < min:
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, min)
	case n > MaxPasswordLength:
		return fmt.Errorf("%w: use at most %d characters", ErrWeakPassword, MaxPasswordLength)
	}
	return nil
}

// hashing bounds the hashes computed at once: each holds Argon2.Memory for
// its whole run, and a burst of sign-ins must not exhaust a small host.
type hashing struct {
	params Argon2
	slots  chan struct{}
	dummy  func() string
	// took reports how long one hash or verification took, its wait for a
	// slot included.
	took func(ctx context.Context, operation string, d time.Duration)
}

func (h *hashing) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hashing) release() { <-h.slots }

// timed starts the clock of one operation and returns the function that
// reports it.
func (h *hashing) timed(ctx context.Context, operation string) func() {
	began := time.Now()
	return func() { h.took(ctx, operation, time.Since(began)) }
}

func (h *hashing) hash(ctx context.Context, password string) (string, error) {
	defer h.timed(ctx, hashHash)()
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return hashPassword(h.params, password)
}

// verify checks password against phc. An empty phc, an account without a
// password, costs the same as a real one and never matches, so the answer
// and its timing say nothing about whether the account exists.
func (h *hashing) verify(ctx context.Context, phc, password string) (ok, stale bool, err error) {
	defer h.timed(ctx, hashVerify)()
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	if phc == "" {
		_, _, err := verifyPassword(h.dummy(), password)
		return false, false, err
	}
	ok, p, err := verifyPassword(phc, password)
	return ok, ok && p != h.params, err
}
