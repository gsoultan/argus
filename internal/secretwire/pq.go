package secretwire

import (
	"crypto/ecdh"
	"crypto/hpke"
	"errors"
)

// Format v2 seals with HPKE (RFC 9180): the ML-KEM-768 + X25519 hybrid KEM,
// HKDF-SHA256 and AES-256-GCM. The hybrid stays secret as long as either half
// does: X25519 against today's attacks, ML-KEM-768 (NIST FIPS 203) against a
// quantum computer that could later break X25519 on a recorded exchange.
//
// Layout: version (2) | encapsulated key | ciphertext.
const sealVersionPQ = 2

// sealInfoPQ is HPKE's info: what the derived key is for.
const sealInfoPQ = "argus-secret seal v2"

var pqKEM = hpke.MLKEM768X25519()

// PQPublicKeySize is the size of an ML-KEM-768 + X25519 public key.
const PQPublicKeySize = 1184 + 32

// pqEncSize is the size of the encapsulated key in a v2 seal.
const pqEncSize = 1088 + 32

// NewPQKey makes an ML-KEM-768 + X25519 key pair.
func NewPQKey() (hpke.PrivateKey, error) { return pqKEM.GenerateKey() }

// PQPrivateKey reads a key made by NewPQKey, as its Bytes gave it.
func PQPrivateKey(b []byte) (hpke.PrivateKey, error) { return pqKEM.NewPrivateKey(b) }

// SealPQ encrypts plain to pub, bound to aad, in format v2.
func SealPQ(pub hpke.PublicKey, plain, aad []byte) ([]byte, error) {
	if pub == nil || pub.KEM().ID() != pqKEM.ID() {
		return nil, errors.New("recipient must be an ML-KEM-768+X25519 key")
	}
	enc, s, err := hpke.NewSender(pub, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(sealInfoPQ))
	if err != nil {
		return nil, err
	}
	ct, err := s.Seal(aad, plain)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(enc)+len(ct))
	out = append(out, sealVersionPQ)
	out = append(out, enc...)
	return append(out, ct...), nil
}

// OpenPQ reverses SealPQ. It opens format v2 only: a caller that asked for a
// post-quantum seal never accepts a v1 answer.
func OpenPQ(priv hpke.PrivateKey, sealed, aad []byte) ([]byte, error) {
	if priv == nil || len(sealed) < 1+pqEncSize+16 || sealed[0] != sealVersionPQ {
		return nil, errOpen
	}
	r, err := hpke.NewRecipient(sealed[1:1+pqEncSize], priv, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(sealInfoPQ))
	if err != nil {
		return nil, errOpen
	}
	plain, err := r.Open(aad, sealed[1+pqEncSize:])
	if err != nil {
		return nil, errOpen
	}
	return plain, nil
}

// SealVersion is the format a sealed value says it is in: 1 (X25519) or 2
// (post-quantum). 0 is not a sealed value.
func SealVersion(sealed []byte) int {
	if len(sealed) == 0 || (sealed[0] != sealVersion && sealed[0] != sealVersionPQ) {
		return 0
	}
	return int(sealed[0])
}

// OpenEither opens a value sealed to either of a server's keys: v1 to x, v2
// to pq. Whether v1 is still accepted is the caller's to decide, by
// SealVersion, before calling.
func OpenEither(x *ecdh.PrivateKey, pq hpke.PrivateKey, sealed, aad []byte) ([]byte, error) {
	switch SealVersion(sealed) {
	case sealVersion:
		if x == nil {
			return nil, errOpen
		}
		return OpenSealed(x, sealed, aad)
	case sealVersionPQ:
		return OpenPQ(pq, sealed, aad)
	}
	return nil, errOpen
}
