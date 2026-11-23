// Package seal makes values that carry a payload through a browser and come
// back once: a form that must take effect once, or a passkey ceremony. A
// value is stamped with when it was made and signed with a key of this
// process, so one made before a restart, whose use the process no longer
// remembers, is not taken after it.
package seal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUsed is a value that was taken before.
	ErrUsed = errors.New("seal: already used")
	// ErrExpired is a value that is malformed, was made for another
	// purpose, is older than the TTL or was made by another process.
	ErrExpired = errors.New("seal: expired")
)

// skew is how far in the future a value's stamp may be.
const skew = time.Minute

const stampLen = 8

// Once makes and takes single-use values. It is safe for concurrent use.
type Once struct {
	key [32]byte
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	used map[string]time.Time // value -> when it was made
}

// NewOnce returns a Once whose values live for ttl by the clock now.
func NewOnce(ttl time.Duration, now func() time.Time) *Once {
	o := &Once{ttl: ttl, now: now, used: map[string]time.Time{}}
	_, _ = rand.Read(o.key[:]) // crypto/rand panics rather than fail
	return o
}

// Seal returns a value that carries payload, bound to purpose: Open takes
// it once, for that purpose alone.
func (o *Once) Seal(purpose string, payload []byte) string {
	body := make([]byte, stampLen+len(payload))
	binary.BigEndian.PutUint64(body, uint64(o.now().Unix()))
	copy(body[stampLen:], payload)
	enc := base64.RawURLEncoding.EncodeToString(body)
	return enc + "." + base64.RawURLEncoding.EncodeToString(o.mac(purpose, enc))
}

// Open returns the payload of a value Seal made for purpose, and takes it:
// ErrUsed when it was taken before, ErrExpired when it is not a live value
// for purpose.
func (o *Once) Open(purpose, value string) ([]byte, error) {
	enc, sig, ok := strings.Cut(value, ".")
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(mac, o.mac(purpose, enc)) {
		return nil, ErrExpired
	}
	body, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(body) < stampLen {
		return nil, ErrExpired
	}
	now := o.now()
	made := time.Unix(int64(binary.BigEndian.Uint64(body[:stampLen])), 0)
	if now.Sub(made) > o.ttl || made.After(now.Add(skew)) {
		return nil, ErrExpired
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for v, at := range o.used {
		if now.Sub(at) > o.ttl {
			delete(o.used, v)
		}
	}
	if _, taken := o.used[value]; taken {
		return nil, ErrUsed
	}
	o.used[value] = made
	return body[stampLen:], nil
}

func (o *Once) mac(purpose, body string) []byte {
	h := hmac.New(sha256.New, o.key[:])
	h.Write([]byte("kazari seal\x00" + purpose + "\x00" + body))
	return h.Sum(nil)
}
