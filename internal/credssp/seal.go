package credssp

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"fmt"
)

// Magic constants from MS-NLMP 3.4.5.2 and 3.4.5.3.
//
// The trailing NUL is part of each string and is load-bearing: derive a key
// without it and everything still works locally while nothing interoperates.
const (
	clientSealMagic = "session key to client-to-server sealing key magic constant\x00"
	serverSealMagic = "session key to server-to-client sealing key magic constant\x00"
	clientSignMagic = "session key to client-to-server signing key magic constant\x00"
	serverSignMagic = "session key to server-to-client signing key magic constant\x00"
)

// ErrBadSignature reports a message whose signature does not verify.
var ErrBadSignature = errors.New("NTLM message signature does not verify")

// Sealer signs and encrypts CredSSP messages under the negotiated session key.
//
// Stateful on purpose, and in two ways that both matter. The RC4 handles run as
// one continuous keystream across the whole exchange, so a fresh cipher per
// message would reuse keystream and leak plaintext. The sequence numbers are
// part of each signature, so they must advance in lockstep with the peer or
// every message after the first fails to verify.
//
// Not safe for concurrent use: CredSSP is a strictly ordered exchange, and
// sealing two messages at once would produce two signatures claiming the same
// sequence number.
type Sealer struct {
	sendSeal *rc4.Cipher
	recvSeal *rc4.Cipher
	sendSign []byte
	recvSign []byte
	sendSeq  uint32
	recvSeq  uint32
	// keyExchange records whether the checksum is itself RC4'd, which depends
	// on a flag negotiated in the challenge rather than on a fixed choice.
	keyExchange bool
}

// NewSealer derives the four directional keys from the exported session key.
//
// asClient selects which direction is "send": Argus authenticates to a Windows
// host as a client, so the client keys are its outbound ones. Getting this
// backwards produces a sealer that encrypts perfectly and cannot be read by
// anyone, which looks like a corrupt message rather than a swapped key.
func NewSealer(exportedSessionKey []byte, flags uint32, asClient bool) (*Sealer, error) {
	if len(exportedSessionKey) != 16 {
		return nil, fmt.Errorf("session key is %d bytes, want 16", len(exportedSessionKey))
	}

	clientSeal := deriveKey(exportedSessionKey, clientSealMagic)
	serverSeal := deriveKey(exportedSessionKey, serverSealMagic)
	clientSign := deriveKey(exportedSessionKey, clientSignMagic)
	serverSign := deriveKey(exportedSessionKey, serverSignMagic)

	sendSealKey, recvSealKey := clientSeal, serverSeal
	sendSignKey, recvSignKey := clientSign, serverSign
	if !asClient {
		sendSealKey, recvSealKey = serverSeal, clientSeal
		sendSignKey, recvSignKey = serverSign, clientSign
	}

	send, err := rc4.NewCipher(sendSealKey)
	if err != nil {
		return nil, err
	}
	recv, err := rc4.NewCipher(recvSealKey)
	if err != nil {
		return nil, err
	}

	return &Sealer{
		sendSeal:    send,
		recvSeal:    recv,
		sendSign:    sendSignKey,
		recvSign:    recvSignKey,
		keyExchange: flags&NegotiateKeyExchange != 0,
	}, nil
}

// deriveKey is MD5(sessionKey || magic).
func deriveKey(sessionKey []byte, magic string) []byte {
	h := md5.New()
	h.Write(sessionKey)
	h.Write([]byte(magic))
	return h.Sum(nil)
}

// Seal encrypts a message and returns signature followed by ciphertext, which
// is the order CredSSP puts them on the wire.
func (s *Sealer) Seal(plaintext []byte) []byte {
	// Order is load-bearing. MS-NLMP 3.4.3 seals the message and then MACs it,
	// and both draw from the same RC4 handle — so doing the MAC first shifts
	// every later keystream byte. The result still verifies against another
	// implementation that made the same mistake, and fails against Windows.
	//
	// The checksum is computed over the plaintext, not the ciphertext: signing
	// the ciphertext would verify against itself while detecting nothing about
	// the message it supposedly covers.
	sealed := make([]byte, len(plaintext))
	s.sendSeal.XORKeyStream(sealed, plaintext)

	checksum := s.mac(s.sendSign, s.sendSeq, plaintext, s.sendSeal)

	out := make([]byte, 0, 16+len(sealed))
	out = binary.LittleEndian.AppendUint32(out, 1) // version
	out = append(out, checksum...)
	out = binary.LittleEndian.AppendUint32(out, s.sendSeq)
	out = append(out, sealed...)

	s.sendSeq++
	return out
}

// Unseal decrypts and verifies a message.
func (s *Sealer) Unseal(message []byte) ([]byte, error) {
	if len(message) < 16 {
		return nil, fmt.Errorf("%w: %d bytes, too short to hold a signature",
			ErrMalformed, len(message))
	}
	signature, sealed := message[:16], message[16:]

	plaintext := make([]byte, len(sealed))
	s.recvSeal.XORKeyStream(plaintext, sealed)

	// The sequence number in the signature is the peer's claim; the checksum is
	// computed over our own counter, so a replayed or reordered message fails
	// here rather than being accepted at the wrong position.
	want := s.mac(s.recvSign, s.recvSeq, plaintext, s.recvSeal)
	if !hmac.Equal(signature[4:12], want) {
		return nil, ErrBadSignature
	}
	if got := binary.LittleEndian.Uint32(signature[12:16]); got != s.recvSeq {
		return nil, fmt.Errorf("%w: sequence %d, expected %d", ErrBadSignature, got, s.recvSeq)
	}

	s.recvSeq++
	return plaintext, nil
}

// mac computes the eight-byte checksum for a message.
//
// With key exchange negotiated the checksum is itself passed through the same
// RC4 stream, which advances it — so this must be called exactly once per
// message and in the same order as the sealing, or the two sides desynchronise.
func (s *Sealer) mac(signKey []byte, seq uint32, message []byte, seal *rc4.Cipher) []byte {
	h := hmac.New(md5.New, signKey)
	var seqBytes [4]byte
	binary.LittleEndian.PutUint32(seqBytes[:], seq)
	h.Write(seqBytes[:])
	h.Write(message)
	checksum := h.Sum(nil)[:8]

	if s.keyExchange {
		out := make([]byte, 8)
		seal.XORKeyStream(out, checksum)
		return out
	}
	return checksum
}

// hmacMD5 is exported within the package for the server-side half of tests,
// which must recompute what a real target would.
func hmacMD5(key, data []byte) []byte {
	h := hmac.New(md5.New, key)
	h.Write(data)
	return h.Sum(nil)
}
