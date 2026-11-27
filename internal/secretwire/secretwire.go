// Package secretwire is what argus-secret and its callers both run: sealing a
// value to a one-time X25519 key, and the shapes of the data-plane requests.
//
// Shared because the agent opens what argus-secret seals, and the two must
// agree byte for byte. Standard library only, so linking it pulls nothing of
// argus-secret's store into the agent.
package secretwire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// sealVersion is the first byte of everything sealed to a public key, so the
// format can change without an old sealed value ever being read as a new one.
const sealVersion = 1

// sealInfo names what the derived key is for; a key derived for one purpose
// can never be the key of another.
const sealInfo = "argus-secret seal v1"

const keySize = 32

var errOpen = errors.New("cannot be opened with this key")

// SealTo encrypts plain so that only the holder of recipient's private key can
// read it: an ephemeral X25519 exchange, HKDF-SHA256, then AES-256-GCM.
//
// Layout: version | ephemeral public key (32) | nonce (12) | ciphertext.
// aad must be supplied again to open it, and says what the value is.
func SealTo(recipient *ecdh.PublicKey, plain, aad []byte) ([]byte, error) {
	if recipient == nil || recipient.Curve() != ecdh.X25519() {
		return nil, errors.New("recipient must be an X25519 public key")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	key, err := sealKey(shared, eph.PublicKey().Bytes(), recipient.Bytes())
	if err != nil {
		return nil, err
	}
	body, err := seal(key, plain, aad)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+32+len(body))
	out = append(out, sealVersion)
	out = append(out, eph.PublicKey().Bytes()...)
	return append(out, body...), nil
}

// OpenSealed reverses SealTo with the recipient's private key.
func OpenSealed(priv *ecdh.PrivateKey, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 1+32 || sealed[0] != sealVersion {
		return nil, errOpen
	}
	ephPub, err := ecdh.X25519().NewPublicKey(sealed[1:33])
	if err != nil {
		return nil, errOpen
	}
	shared, err := priv.ECDH(ephPub)
	if err != nil {
		return nil, errOpen
	}
	key, err := sealKey(shared, sealed[1:33], priv.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	return open(key, sealed[33:], aad)
}

// sealKey binds the derived key to both public keys, so a sealed value cannot
// be replayed to a different recipient.
func sealKey(shared, ephPub, recipientPub []byte) ([]byte, error) {
	salt := make([]byte, 0, len(ephPub)+len(recipientPub))
	salt = append(salt, ephPub...)
	salt = append(salt, recipientPub...)
	return hkdf.Key(sha256.New, shared, salt, sealInfo, keySize)
}

// ParseRecipient reads the key a caller wants a value sealed to, base64url
// without padding: a 32-byte X25519 key (format v1), or an ML-KEM-768 +
// X25519 hybrid key (format v2, post-quantum). Which one it is decides how the
// value is sealed, so a caller that sent a hybrid key only ever gets v2 back.
func ParseRecipient(s string) (Recipient, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || (len(raw) != 32 && len(raw) != PQPublicKeySize) {
		return nil, errors.New("recipient must be a 32-byte X25519 key or an ML-KEM-768+X25519 key, base64url without padding")
	}
	if len(raw) == PQPublicKeySize {
		pub, err := pqKEM.NewPublicKey(raw)
		if err != nil {
			return nil, errors.New("recipient is not a usable ML-KEM-768+X25519 key")
		}
		return pqRecipient{pub}, nil
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("recipient is not a usable X25519 public key")
	}
	return x25519Recipient{pub}, nil
}

// Recipient is a public key a value can be sealed to.
type Recipient interface {
	// Seal encrypts plain so that only the holder of the private key can
	// read it, bound to aad.
	Seal(plain, aad []byte) ([]byte, error)
	// PostQuantum says the seal stays secret against a quantum computer.
	PostQuantum() bool
}

type x25519Recipient struct{ pub *ecdh.PublicKey }

func (r x25519Recipient) Seal(plain, aad []byte) ([]byte, error) { return SealTo(r.pub, plain, aad) }
func (x25519Recipient) PostQuantum() bool                        { return false }

type pqRecipient struct{ pub hpke.PublicKey }

func (r pqRecipient) Seal(plain, aad []byte) ([]byte, error) { return SealPQ(r.pub, plain, aad) }
func (pqRecipient) PostQuantum() bool                        { return true }

// EncodeKey is how public keys travel in JSON: base64url without padding.
func EncodeKey(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ReadAAD is what a value read from argus-secret is sealed with. The caller
// supplies the same to open it, so a sealed answer for one secret, version or
// field cannot be passed off as another.
func ReadAAD(name string, version int, field string) []byte {
	return fmt.Appendf(nil, "argus-secret read v1\x00%s\x00%d\x00%s", name, version, field)
}

// WriteAAD is what a value sent to argus-secret is sealed with: the console
// seals a new value to argus-secret's public key, so argus-control relays it
// without ever holding it.
func WriteAAD(name string) []byte {
	return fmt.Appendf(nil, "argus-secret write v1\x00%s", name)
}

func seal(key, plain, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errOpen
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
	if err != nil {
		return nil, errOpen
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ReadRequest asks for one value, sealed to Recipient.
type ReadRequest struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
	Label   string `json:"label,omitempty"`
	Field   string `json:"field,omitempty"`
	// Recipient is a one-time X25519 public key the caller made for this
	// request, base64url without padding.
	Recipient string `json:"recipient"`
	// Purpose is what the read is for, as the caller says: "argus-agent run".
	Purpose string `json:"purpose,omitempty"`
}

// ReadResponse carries the sealed value. Open it with the recipient's private
// key and ReadAAD(name, version, field).
type ReadResponse struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Field   string `json:"field,omitempty"`
	Sealed  []byte `json:"sealed"`
}

// Listed is a secret as a reader sees it in a list: no value, no history.
type Listed struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Current   int       `json:"current"`
	Fields    []string  `json:"fields,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	CanRead   bool      `json:"canRead"`
}
