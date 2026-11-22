// Package passkeytest is a software authenticator for tests: it answers the
// options a page carries as a browser with a synced passkey would, with real
// keys and signatures, so the ceremonies are checked end to end.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Flags of authenticator data.
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagBE = 0x08 // backup eligible
	flagBS = 0x10 // backed up
	flagAT = 0x40 // attested credential data follows
)

// Passkey is one credential the authenticator holds.
type Passkey struct {
	ID     []byte
	RPID   string
	UserID []byte
	key    *ecdsa.PrivateKey
	// Count is the signature counter; 0 stays 0, as synced passkeys do.
	Count uint32
}

// Authenticator holds passkeys and answers ceremonies. Its fields change
// what it reports, to test refusals.
type Authenticator struct {
	Passkeys []*Passkey
	// NoUV leaves the user-verified flag unset.
	NoUV bool
}

// ErrNoPasskey is a sign-in for which the authenticator holds no passkey.
var ErrNoPasskey = errors.New("passkeytest: no passkey for this relying party")

var b64 = base64.RawURLEncoding

type creationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Exclude []struct {
		ID string `json:"id"`
	} `json:"excludeCredentials"`
}

type requestOptions struct {
	Challenge string `json:"challenge"`
	RPID      string `json:"rpId"`
}

// ErrExcluded is a registration for which the authenticator already holds
// an excluded passkey.
var ErrExcluded = errors.New("passkeytest: a passkey here is excluded")

// Create answers a registration's options as a page of origin would, and
// keeps the new passkey. It returns the JSON the page posts.
func (a *Authenticator) Create(options, origin string) (string, error) {
	var o creationOptions
	if err := json.Unmarshal([]byte(options), &o); err != nil {
		return "", fmt.Errorf("passkeytest: options: %w", err)
	}
	for _, ex := range o.Exclude {
		for _, p := range a.Passkeys {
			if ex.ID == b64.EncodeToString(p.ID) {
				return "", ErrExcluded
			}
		}
	}
	userID, err := b64.DecodeString(o.User.ID)
	if err != nil {
		return "", fmt.Errorf("passkeytest: user id: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	p := &Passkey{ID: make([]byte, 16), RPID: o.RP.ID, UserID: userID, key: key}
	_, _ = rand.Read(p.ID)

	client := clientData("webauthn.create", o.Challenge, origin)
	auth := a.authData(p, flagAT)
	auth = append(auth, make([]byte, 16)...) // AAGUID
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(p.ID)))
	auth = append(auth, p.ID...)
	auth = append(auth, coseKey(&key.PublicKey)...)

	att := []byte{0xa3}
	att = append(att, text("fmt")...)
	att = append(att, text("none")...)
	att = append(att, text("attStmt")...)
	att = append(att, 0xa0)
	att = append(att, text("authData")...)
	att = append(att, bytesHead(len(auth))...)
	att = append(att, auth...)

	a.Passkeys = append(a.Passkeys, p)
	return encode(map[string]any{
		"id": b64.EncodeToString(p.ID), "rawId": b64.EncodeToString(p.ID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(client),
			"attestationObject": b64.EncodeToString(att),
			"transports":        []string{"internal", "hybrid"},
		},
	})
}

// Get answers a sign-in's options with the newest passkey it holds for
// their relying party, as a page of origin would.
func (a *Authenticator) Get(options, origin string) (string, error) {
	var o requestOptions
	if err := json.Unmarshal([]byte(options), &o); err != nil {
		return "", fmt.Errorf("passkeytest: options: %w", err)
	}
	for _, p := range slices.Backward(a.Passkeys) {
		if p.RPID == o.RPID {
			return a.Sign(p, o.Challenge, origin)
		}
	}
	return "", ErrNoPasskey
}

// Challenge returns the challenge of a ceremony's options.
func Challenge(options string) string {
	var o requestOptions
	_ = json.Unmarshal([]byte(options), &o)
	return o.Challenge
}

// Sign answers a sign-in with challenge with passkey p.
func (a *Authenticator) Sign(p *Passkey, challenge, origin string) (string, error) {
	if p.Count > 0 {
		p.Count++
	}
	client := clientData("webauthn.get", challenge, origin)
	auth := a.authData(p, 0)
	sum := sha256.Sum256(client)
	digest := sha256.Sum256(append(append([]byte{}, auth...), sum[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, p.key, digest[:])
	if err != nil {
		return "", err
	}
	return encode(map[string]any{
		"id": b64.EncodeToString(p.ID), "rawId": b64.EncodeToString(p.ID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(client),
			"authenticatorData": b64.EncodeToString(auth),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(p.UserID),
		},
	})
}

func (a *Authenticator) authData(p *Passkey, extra byte) []byte {
	rp := sha256.Sum256([]byte(p.RPID))
	flags := byte(flagUP|flagBE|flagBS) | extra
	if !a.NoUV {
		flags |= flagUV
	}
	out := append(rp[:], flags)
	return binary.BigEndian.AppendUint32(out, p.Count)
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// The few CBOR items an attestation object and a COSE key need.

func text(s string) []byte { return append([]byte{0x60 | byte(len(s))}, s...) }

func bytesHead(n int) []byte {
	switch {
	case n < 24:
		return []byte{0x40 | byte(n)}
	case n < 256:
		return []byte{0x58, byte(n)}
	}
	return []byte{0x59, byte(n >> 8), byte(n)}
}

// coseKey is an EC2 P-256 ES256 key: {1: 2, 3: -7, -1: 1, -2: x, -3: y}.
func coseKey(k *ecdsa.PublicKey) []byte {
	raw, _ := k.ECDH()
	point := raw.Bytes() // 0x04 || x || y
	x, y := point[1:33], point[33:65]
	out := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21}
	out = append(out, bytesHead(32)...)
	out = append(out, x...)
	out = append(out, 0x22)
	out = append(out, bytesHead(32)...)
	return append(out, y...)
}
