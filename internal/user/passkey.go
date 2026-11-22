package user

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// Passkeys: the WebAuthn credentials of built-in accounts. The ceremonies
// are the passkey package's; this keeps what they make, and says whose it
// is.

const (
	// MaxPasskeys is how many passkeys a person may hold.
	MaxPasskeys = 20
	// MaxPasskeyName is the longest name a passkey is given, in characters.
	MaxPasskeyName = 100
	// passkeyUserIDBytes is the length of a new passkey user handle; the
	// specification allows 64 at most.
	passkeyUserIDBytes = 32
)

// PasskeyUserID returns the user handle the passkeys of handle are made
// for: the one their passkeys hold, or a new random one when they have
// none, which AddPasskey stores with the first. ErrNotFound, ErrLocalOff.
func (s *Service) PasskeyUserID(ctx context.Context, handle string) ([]byte, error) {
	if !s.opts.LocalAccounts {
		return nil, ErrLocalOff
	}
	id, err := s.store.PasskeyUserID(ctx, handle)
	if err != nil || id != nil {
		return id, err
	}
	id = make([]byte, passkeyUserIDBytes)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("user: generate passkey user handle: %w", err)
	}
	return id, nil
}

// NewPasskey is a passkey a ceremony made, to keep.
type NewPasskey struct {
	// UserID is the user handle the passkey was made for, from
	// PasskeyUserID.
	UserID       []byte
	CredentialID []byte
	Record       []byte
	Name         string // "" names it "Passkey"
}

// AddPasskey keeps a passkey of handle. ErrNotFound, ErrDisabled,
// ErrTooManyPasskeys, ErrPasskeyExists, ErrPasskeyStale, ErrLocalOff.
func (s *Service) AddPasskey(ctx context.Context, handle string, np NewPasskey) (_ *Passkey, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.add_passkey")
	defer func() { endSpan(span, err) }()
	if !s.opts.LocalAccounts {
		return nil, ErrLocalOff
	}
	name := strings.Join(strings.Fields(np.Name), " ")
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > MaxPasskeyName {
		name = string([]rune(name)[:MaxPasskeyName])
	}
	p := &Passkey{UserHandle: handle, Name: name, CredentialID: np.CredentialID, Record: np.Record, CreatedAt: s.now()}
	if err := s.store.CreatePasskey(ctx, p, np.UserID, MaxPasskeys); err != nil {
		return nil, err
	}
	s.record(ctx, actorFrom(ctx, handle), audit.PasskeyCreate, handle, map[string]string{
		"handle": handle, "id": strconv.FormatInt(p.ID, 10), "name": p.Name,
	})
	return p, nil
}

// Passkeys returns the passkeys of handle, oldest first. ErrNotFound.
func (s *Service) Passkeys(ctx context.Context, handle string) ([]Passkey, error) {
	return s.store.ListPasskeys(ctx, handle)
}

// RemovePasskey deletes a passkey of handle. ErrPasskeyNotFound.
func (s *Service) RemovePasskey(ctx context.Context, handle string, id int64) (err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "user.remove_passkey")
	defer func() { endSpan(span, err) }()
	p, err := s.store.DeletePasskey(ctx, handle, id)
	if err != nil {
		return err
	}
	s.record(ctx, actorFrom(ctx, handle), audit.PasskeyDelete, handle, map[string]string{
		"handle": handle, "id": strconv.FormatInt(p.ID, 10), "name": p.Name,
	})
	return nil
}

// PasskeyByCredential returns the passkey a sign-in names, its person and
// the user handle their passkeys hold. ErrPasskeyNotFound for an unknown
// passkey or a disabled person, and on an instance whose built-in accounts
// are off.
func (s *Service) PasskeyByCredential(ctx context.Context, credentialID []byte) (*Passkey, *User, []byte, error) {
	if !s.opts.LocalAccounts {
		return nil, nil, nil, ErrPasskeyNotFound
	}
	p, u, userID, err := s.store.GetPasskey(ctx, credentialID)
	if err != nil {
		return nil, nil, nil, err
	}
	if u.DisabledAt != nil {
		return nil, nil, nil, ErrPasskeyNotFound
	}
	return p, u, userID, nil
}

// PasskeyUsed keeps the record a sign-in with passkey id updated. A
// passkey removed since is not an error.
func (s *Service) PasskeyUsed(ctx context.Context, id int64, record []byte) error {
	err := s.store.TouchPasskey(ctx, id, record, s.now())
	if errors.Is(err, ErrPasskeyNotFound) {
		return nil
	}
	return err
}
