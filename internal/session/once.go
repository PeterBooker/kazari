package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// OnceField is the form field that carries a single-use value.
const OnceField = "once"

// onceTTL is how long a form that makes something may stay open, and how
// long a used value is remembered.
const onceTTL = 12 * time.Hour

var (
	// ErrFormUsed: the form was sent before, such as by a reload of the page
	// that answered it.
	ErrFormUsed = errors.New("session: form already sent")
	// ErrFormExpired: the form's single-use value is missing, is another
	// session's, or is older than the form may stay open or than the server.
	ErrFormExpired = errors.New("session: form expired")
)

// once is what makes a form's POST take effect once: a key for signing
// values and the values used.
type once struct {
	key  [32]byte
	mu   sync.Mutex
	used map[string]time.Time // value -> when it was made
}

func newOnce() *once {
	o := &once{used: map[string]time.Time{}}
	// A key per process: a value made before a restart, whose use the
	// server no longer remembers, is not taken after it.
	_, _ = rand.Read(o.key[:]) // crypto/rand panics rather than fail
	return o
}

const (
	onceStamp = 8
	onceRand  = 16
	onceBody  = onceStamp + onceRand
)

// Once returns a new value for a form whose POST must take effect once,
// such as one that makes a secret shown once: Claim takes it the first time
// and refuses it after. Every page load gets another.
func (s *Session) Once() string {
	var b [onceBody + sha256.Size]byte
	binary.BigEndian.PutUint64(b[:onceStamp], uint64(s.m.opts.Now().Unix()))
	_, _ = rand.Read(b[onceStamp:onceBody])
	copy(b[onceBody:], s.onceMAC(b[:onceBody]))
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Claim takes the single-use value of a form: ErrFormUsed when it was taken
// before, ErrFormExpired when it is not a live value of this session.
func (s *Session) Claim(value string) error {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(b) != onceBody+sha256.Size || !hmac.Equal(b[onceBody:], s.onceMAC(b[:onceBody])) {
		return ErrFormExpired
	}
	now := s.m.opts.Now()
	made := time.Unix(int64(binary.BigEndian.Uint64(b[:onceStamp])), 0)
	if now.Sub(made) > onceTTL || made.After(now.Add(time.Minute)) {
		return ErrFormExpired
	}
	o := s.m.once
	o.mu.Lock()
	defer o.mu.Unlock()
	for v, at := range o.used {
		if now.Sub(at) > onceTTL {
			delete(o.used, v)
		}
	}
	if _, ok := o.used[value]; ok {
		return ErrFormUsed
	}
	o.used[value] = made
	return nil
}

// onceMAC binds a value to the process's key and to the session.
func (s *Session) onceMAC(body []byte) []byte {
	mac := hmac.New(sha256.New, s.m.once.key[:])
	mac.Write([]byte("kazari once " + s.host + "\x00" + s.token + "\x00"))
	mac.Write(body)
	return mac.Sum(nil)
}
