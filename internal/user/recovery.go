package user

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// Recovery codes: single-use codes a person keeps somewhere safe and uses to
// set a new password when they lose theirs, without asking an admin.

const (
	// RecoveryCodeCount is how many codes a new set holds.
	RecoveryCodeCount = 10
	// recoveryCodeBytes is 80 bits, 16 characters of base32.
	recoveryCodeBytes = 10
	recoveryCodeLen   = 16
	recoveryGroup     = 4
)

// NewRecoveryCodes makes a new set of recovery codes for handle, replacing
// any earlier set, and returns them in the form a person copies:
// abcd-efgh-ijkl-mnop. They are never available again. current is the
// person's password, which making codes asks for again. ErrBadPassword,
// ErrBusy, ErrLocalOff.
func (s *Service) NewRecoveryCodes(ctx context.Context, handle, current string) (_ []string, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.new_recovery_codes")
	defer func() { endSpan(span, err) }()
	if err := s.ConfirmPassword(ctx, handle, current); err != nil {
		return nil, err
	}
	codes := make([]string, RecoveryCodeCount)
	hashes := make([]string, RecoveryCodeCount)
	for i := range codes {
		var b [recoveryCodeBytes]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("user: generate recovery code: %w", err)
		}
		raw := keyEncoding.EncodeToString(b[:])
		codes[i], hashes[i] = groupCode(raw), HashKey(raw)
	}
	if err := s.store.ReplaceRecoveryCodes(ctx, handle, hashes, s.now()); err != nil {
		return nil, err
	}
	s.record(ctx, actorFrom(ctx, handle), audit.RecoveryCreate, handle, map[string]string{
		"handle": handle, "codes": strconv.Itoa(RecoveryCodeCount),
	})
	return codes, nil
}

// groupCode splits a code into groups of four for reading.
func groupCode(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i += recoveryGroup {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(raw[i:min(i+recoveryGroup, len(raw))])
	}
	return b.String()
}

// normalizeCode returns a code as typed in the form it is hashed in, or ""
// when it cannot be one: case, spaces and hyphens do not matter.
func normalizeCode(code string) string {
	code = strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(code)))
	if len(code) != recoveryCodeLen || !isBase32(code) {
		return ""
	}
	return code
}

// Recover sets a new password for the person login names, by handle or by
// email, with one of their recovery codes, which it uses up. Like a reset
// link it signs nobody in and ends the person's browser sessions; their
// keys and connected apps keep working. Every refusal of the name or the
// code is ErrBadRecoveryCode. ErrWeakPassword, ErrBusy, ErrLocalOff.
func (s *Service) Recover(ctx context.Context, login, code, password string) (_ *User, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.recover")
	defer func() { endSpan(span, err) }()
	if !s.opts.LocalAccounts {
		return nil, ErrLocalOff
	}
	if err := checkPolicy(password, s.opts.MinPasswordLength); err != nil {
		return nil, err
	}
	u, err := s.byLogin(ctx, login)
	if err != nil {
		return nil, err
	}
	raw := normalizeCode(code)
	if u == nil || u.DisabledAt != nil || raw == "" {
		return nil, ErrBadRecoveryCode
	}
	// Looked up before the password is hashed, so a wrong code costs a
	// query and not 64 MiB. The use below checks it again.
	ok, err := s.store.HasRecoveryCode(ctx, u.Handle, HashKey(raw))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBadRecoveryCode
	}
	hash, err := s.hashing.hash(ctx, password)
	if err != nil {
		return nil, err
	}
	now := s.now()
	left, err := s.store.UseRecoveryCode(ctx, u.Handle, HashKey(raw), now, hash)
	if err != nil {
		return nil, err
	}
	s.record(ctx, u.Handle, audit.RecoveryUse, u.Handle, map[string]string{"handle": u.Handle, "left": strconv.Itoa(left)})
	s.record(ctx, u.Handle, audit.PasswordSet, u.Handle, map[string]string{"handle": u.Handle, "via": "recovery"})
	if err := s.endSessions(ctx, u.Handle, 0); err != nil {
		return nil, fmt.Errorf("user: recover %q: %w", u.Handle, err)
	}
	return u, nil
}

// ConfirmPassword checks handle's password again, for a step that adds a
// way to sign in: a stolen session alone must not add one. It records no
// sign-in. ErrBadPassword, ErrBusy, ErrLocalOff.
func (s *Service) ConfirmPassword(ctx context.Context, handle, password string) error {
	if !s.opts.LocalAccounts {
		return ErrLocalOff
	}
	if strings.Contains(handle, "@") {
		return ErrBadPassword
	}
	_, _, err := s.verify(ctx, handle, password)
	return err
}
