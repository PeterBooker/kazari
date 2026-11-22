// Package passkey runs the WebAuthn ceremonies of built-in accounts: adding
// a passkey to an account and signing in with one. The user package keeps
// the passkeys; this makes and checks them.
//
// A ceremony needs no state on the server between the page and its answer.
// The page carries the challenge and what goes with it in a token sealed
// with a key of this process, bound to what the ceremony is for; the answer
// sends the token back, and the challenge is taken once. A page that was
// open across a restart, or longer than Options.TTL, starts again.
//
// Every passkey is discoverable and verifies its user (a fingerprint, a
// face or a PIN), so a sign-in needs no name and no password.
package passkey

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/net/publicsuffix"

	"github.com/PeterBooker/kazari/internal/user"
)

var (
	// ErrExpired is an answer whose ceremony is unknown to this process,
	// was used, is another's, or is older than Options.TTL.
	ErrExpired = errors.New("passkey: ceremony expired")
	// ErrRejected is an answer that does not verify, or names a passkey
	// that is unknown, removed or another person's, never told apart.
	ErrRejected = errors.New("passkey: not accepted")
)

// Supported reports whether passkeys can work for an instance on
// baseDomain. Browsers make a passkey for a registrable domain that both
// the apex and the sign-in host end in; localhost, and a public suffix such
// as github.io, are not one.
func Supported(baseDomain string) bool {
	_, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(baseDomain, "."))
	return err == nil && !strings.EqualFold(baseDomain, "localhost")
}

// DefaultTTL is how long a page may stay open before its ceremony expires.
const DefaultTTL = 15 * time.Minute

// Users keeps the passkeys. user.Service implements it.
type Users interface {
	PasskeyUserID(ctx context.Context, handle string) ([]byte, error)
	Passkeys(ctx context.Context, handle string) ([]user.Passkey, error)
	AddPasskey(ctx context.Context, handle string, p user.NewPasskey) (*user.Passkey, error)
	PasskeyByCredential(ctx context.Context, credentialID []byte) (*user.Passkey, *user.User, []byte, error)
	PasskeyUsed(ctx context.Context, id int64, record []byte) error
}

// Options configures a Service.
type Options struct {
	// RPID is the domain passkeys are made for: the base domain, which the
	// apex and the sign-in host share.
	RPID string
	// Origins are the pages' origins, such as https://art.example.com and
	// https://mcp.art.example.com. An answer from any other is refused.
	Origins []string
	// TTL is how long a ceremony lasts (default DefaultTTL).
	TTL time.Duration
	Now func() time.Time
}

// Service runs the ceremonies.
type Service struct {
	users Users
	wa    *webauthn.WebAuthn
	opts  Options
	key   [32]byte

	mu   sync.Mutex
	used map[string]time.Time // challenge -> when its ceremony began
}

// New returns a Service, or an error for an RPID or origins WebAuthn
// cannot use.
func New(users Users, opts Options) (*Service, error) {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	required := true
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  opts.RPID,
		RPDisplayName:         "Kazari",
		RPOrigins:             opts.Origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: &required,
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: opts.TTL, TimeoutUVD: opts.TTL},
			Registration: webauthn.TimeoutConfig{Timeout: opts.TTL, TimeoutUVD: opts.TTL},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("passkey: %w", err)
	}
	s := &Service{users: users, wa: wa, opts: opts, used: map[string]time.Time{}}
	// A key per process: a page made before a restart starts again.
	_, _ = rand.Read(s.key[:])
	return s, nil
}

// Ceremony is what a page carries: Options for navigator.credentials, as
// JSON, and Token, which goes back with the answer.
type Ceremony struct {
	Options string
	Token   string
}

// What a ceremony is for, sealed into its token.
const (
	kindCreate = "create"
	kindGet    = "get"
)

// account is a person as WebAuthn sees them.
type account struct {
	id          []byte
	name, shown string
	creds       []webauthn.Credential
}

func (a *account) WebAuthnID() []byte                         { return a.id }
func (a *account) WebAuthnName() string                       { return a.name }
func (a *account) WebAuthnDisplayName() string                { return a.shown }
func (a *account) WebAuthnCredentials() []webauthn.Credential { return a.creds }

func accountOf(u *user.User, id []byte) *account {
	shown := u.DisplayName
	if shown == "" {
		shown = u.Handle
	}
	return &account{id: id, name: u.Handle, shown: shown}
}

// Registration begins adding a passkey to u's account. The browser is asked
// not to make a second passkey where u already has one.
func (s *Service) Registration(ctx context.Context, u *user.User) (Ceremony, error) {
	id, err := s.users.PasskeyUserID(ctx, u.Handle)
	if err != nil {
		return Ceremony{}, err
	}
	held, err := s.users.Passkeys(ctx, u.Handle)
	if err != nil {
		return Ceremony{}, err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(held))
	for _, p := range held {
		exclude = append(exclude, protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: p.CredentialID})
	}
	creation, sess, err := s.wa.BeginRegistration(accountOf(u, id), webauthn.WithExclusions(exclude))
	if err != nil {
		return Ceremony{}, fmt.Errorf("passkey: begin registration: %w", err)
	}
	return s.ceremony(kindCreate, u.Handle, creation.Response, sess)
}

// Register checks the browser's answer to a registration of u's and keeps
// the new passkey under name. ErrExpired, ErrRejected, and the errors of
// user.Service.AddPasskey.
func (s *Service) Register(ctx context.Context, u *user.User, token, response, name string) (*user.Passkey, error) {
	sess, err := s.open(kindCreate, u.Handle, token)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(response))
	if err != nil {
		return nil, ErrRejected
	}
	cred, err := s.wa.CreateCredential(accountOf(u, sess.UserID), *sess, parsed)
	if err != nil {
		return nil, ErrRejected
	}
	record, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("passkey: encode credential: %w", err)
	}
	return s.users.AddPasskey(ctx, u.Handle, user.NewPasskey{UserID: sess.UserID, CredentialID: cred.ID, Record: record, Name: name})
}

// SignIn begins a sign-in with any passkey of this instance.
func (s *Service) SignIn() (Ceremony, error) {
	assertion, sess, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return Ceremony{}, fmt.Errorf("passkey: begin sign-in: %w", err)
	}
	return s.ceremony(kindGet, "", assertion.Response, sess)
}

// Verify checks the browser's answer to a sign-in and returns the person
// whose passkey it is. ErrExpired, ErrRejected.
func (s *Service) Verify(ctx context.Context, token, response string) (*user.User, error) {
	sess, err := s.open(kindGet, "", token)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(response))
	if err != nil {
		return nil, ErrRejected
	}
	var (
		key     *user.Passkey
		person  *user.User
		lookErr error
	)
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		p, u, id, err := s.users.PasskeyByCredential(ctx, rawID)
		if err != nil {
			if !errors.Is(err, user.ErrPasskeyNotFound) {
				lookErr = err
			}
			return nil, err
		}
		if !bytes.Equal(id, userHandle) {
			return nil, user.ErrPasskeyNotFound
		}
		var cred webauthn.Credential
		if err := json.Unmarshal(p.Record, &cred); err != nil {
			lookErr = fmt.Errorf("passkey: decode credential %d: %w", p.ID, err)
			return nil, lookErr
		}
		key, person = p, u
		a := accountOf(u, id)
		a.creds = []webauthn.Credential{cred}
		return a, nil
	}
	_, cred, err := s.wa.ValidatePasskeyLogin(handler, *sess, parsed)
	if lookErr != nil {
		return nil, lookErr
	}
	// A counter that went back means a copy of the authenticator exists.
	// Synced passkeys count nothing, so this only ever concerns a security
	// key.
	if err != nil || cred.Authenticator.CloneWarning {
		return nil, ErrRejected
	}
	record, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("passkey: encode credential: %w", err)
	}
	if err := s.users.PasskeyUsed(ctx, key.ID, record); err != nil {
		return nil, err
	}
	return person, nil
}

// ticket is what a ceremony's token seals.
type ticket struct {
	Made    int64                `json:"t"`
	Session webauthn.SessionData `json:"s"`
}

func (s *Service) ceremony(kind, bind string, options any, sess *webauthn.SessionData) (Ceremony, error) {
	opts, err := json.Marshal(options)
	if err != nil {
		return Ceremony{}, fmt.Errorf("passkey: encode options: %w", err)
	}
	payload, err := json.Marshal(ticket{Made: s.opts.Now().Unix(), Session: *sess})
	if err != nil {
		return Ceremony{}, fmt.Errorf("passkey: encode ceremony: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return Ceremony{Options: string(opts), Token: body + "." + base64.RawURLEncoding.EncodeToString(s.mac(kind, bind, body))}, nil
}

// open returns the session a token seals for kind and bind, and takes its
// challenge so that the token works once.
func (s *Service) open(kind, bind, token string) (*webauthn.SessionData, error) {
	body, sig, ok := strings.Cut(token, ".")
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(mac, s.mac(kind, bind, body)) {
		return nil, ErrExpired
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrExpired
	}
	var t ticket
	if err := json.Unmarshal(payload, &t); err != nil || t.Session.Challenge == "" {
		return nil, ErrExpired
	}
	now := s.opts.Now()
	made := time.Unix(t.Made, 0)
	if now.Sub(made) > s.opts.TTL || made.After(now.Add(time.Minute)) {
		return nil, ErrExpired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, at := range s.used {
		if now.Sub(at) > s.opts.TTL {
			delete(s.used, c)
		}
	}
	if _, taken := s.used[t.Session.Challenge]; taken {
		return nil, ErrExpired
	}
	s.used[t.Session.Challenge] = made
	return &t.Session, nil
}

// mac binds a token to this process, what it is for and, for a
// registration, whose account it adds to.
func (s *Service) mac(kind, bind, body string) []byte {
	h := hmac.New(sha256.New, s.key[:])
	h.Write([]byte("kazari passkey\x00" + kind + "\x00" + bind + "\x00" + body))
	return h.Sum(nil)
}
