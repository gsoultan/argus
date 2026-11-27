package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// KeySize is the size of every symmetric key here: AES-256.
const KeySize = 32

var errOpen = errors.New("cannot be opened with this key")

// randomKey returns KeySize random bytes.
func randomKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// Wrapping format v2 is AES-256-GCM made committing: GCM alone lets a crafted
// ciphertext open under more than one key, so a v2 value also carries
//
//	commitment = HMAC-SHA256(Kc, "argus-secret commit v2" || nonce || aad)
//
// and is opened only when it matches. Kc and the AES key are both derived
// from the wrapping key with HKDF-SHA256 under different labels, so the
// commitment says nothing about the AES key. A v2 value opens under exactly
// one key, one nonce and one aad.
//
// Layout: 0xC2 | commitment (32) | nonce (12) | ciphertext and tag.
//
// Format v1 is nonce | ciphertext and tag, plain GCM. It still opens unless
// the store has been re-encrypted (argus-secret rekey), which turns it off.
const (
	wrapV2       = 0xC2
	commitSize   = 32
	commitLabel  = "argus-secret commit v2"
	encKeyInfo   = "argus-secret wrap v2 enc"
	commitKeyInf = "argus-secret wrap v2 commit"
)

// wrap encrypts plain under key in format v2. aad binds the result to what it
// is (the secret, the version, the organization), so a ciphertext copied to
// another row does not open there.
func wrap(key, plain, aad []byte) ([]byte, error) {
	ek, ck, err := wrapKeys(key)
	if err != nil {
		return nil, err
	}
	defer clear(ek)
	defer clear(ck)
	gcm, err := newGCM(ek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+commitSize+len(nonce)+len(plain)+gcm.Overhead())
	out = append(out, wrapV2)
	out = append(out, commitment(ck, nonce, aad)...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, aad), nil
}

// unwrap reverses wrap. legacy allows format v1 as well.
func unwrap(key, sealed, aad []byte, legacy bool) ([]byte, error) {
	if len(sealed) >= 1+commitSize+12+16 && sealed[0] == wrapV2 {
		if plain, err := unwrapV2(key, sealed, aad); err == nil {
			return plain, nil
		}
		// A v1 value whose random nonce began with 0xC2 lands here: one in
		// 256. It fails the commitment and is tried as v1 below.
	}
	if !legacy {
		return nil, errOpen
	}
	return unwrapV1(key, sealed, aad)
}

func unwrapV2(key, sealed, aad []byte) ([]byte, error) {
	ek, ck, err := wrapKeys(key)
	if err != nil {
		return nil, err
	}
	defer clear(ek)
	defer clear(ck)
	c, rest := sealed[1:1+commitSize], sealed[1+commitSize:]
	nonce := rest[:12]
	if !hmac.Equal(c, commitment(ck, nonce, aad)) {
		return nil, errOpen
	}
	gcm, err := newGCM(ek)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, rest[12:], aad)
	if err != nil {
		return nil, errOpen
	}
	return plain, nil
}

func unwrapV1(key, sealed, aad []byte) ([]byte, error) {
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

// isWrapV2 says a stored value is already in format v2.
func isWrapV2(key, sealed, aad []byte) bool {
	if len(sealed) < 1+commitSize+12+16 || sealed[0] != wrapV2 {
		return false
	}
	_, err := unwrapV2(key, sealed, aad)
	return err == nil
}

func wrapKeys(key []byte) (enc, commit []byte, err error) {
	if len(key) != KeySize {
		return nil, nil, fmt.Errorf("key is %d bytes, want %d", len(key), KeySize)
	}
	if enc, err = hkdf.Key(sha256.New, key, nil, encKeyInfo, KeySize); err != nil {
		return nil, nil, err
	}
	if commit, err = hkdf.Key(sha256.New, key, nil, commitKeyInf, KeySize); err != nil {
		return nil, nil, err
	}
	return enc, commit, nil
}

func commitment(ck, nonce, aad []byte) []byte {
	m := hmac.New(sha256.New, ck)
	m.Write([]byte(commitLabel))
	m.Write(nonce)
	m.Write(aad)
	return m.Sum(nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
