package session

import (
	"crypto/rand"
	"errors"
	"time"

	"github.com/PeterBooker/kazari/internal/seal"
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

// Once returns a new value for a form whose POST must take effect once,
// such as one that makes a secret shown once: Claim takes it the first time
// and refuses it after. Every page load gets another.
func (s *Session) Once() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand panics rather than fail
	return s.m.once.Seal(s.oncePurpose(), b[:])
}

// Claim takes the single-use value of a form: ErrFormUsed when it was taken
// before, ErrFormExpired when it is not a live value of this session.
func (s *Session) Claim(value string) error {
	_, err := s.m.once.Open(s.oncePurpose(), value)
	switch {
	case errors.Is(err, seal.ErrUsed):
		return ErrFormUsed
	case err != nil:
		return ErrFormExpired
	}
	return nil
}

// oncePurpose binds a value to the session.
func (s *Session) oncePurpose() string {
	return "once\x00" + s.host + "\x00" + s.token
}
