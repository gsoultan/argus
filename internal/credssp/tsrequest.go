package credssp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"fmt"
)

// Version is the CredSSP version Argus speaks.
//
// Six enables the nonce-based public key binding. Versions below three compute
// the binding over the bare public key, which lets a captured exchange be
// replayed against a different channel; the nonce is what removes that, so
// Argus asks for the version that has it.
const Version = 6

// Binding hash magic strings (MS-CSSP 3.1.5).
const (
	clientToServerBinding = "CredSSP Client-To-Server Binding Hash\x00"
	serverToClientBinding = "CredSSP Server-To-Client Binding Hash\x00"
)

// negoToken wraps one SPNEGO/NTLM token.
type negoToken struct {
	Token []byte `asn1:"explicit,tag:0"`
}

// tsRequest is the CredSSP envelope (MS-CSSP 2.2.1).
type tsRequest struct {
	Version     int         `asn1:"explicit,tag:0"`
	NegoTokens  []negoToken `asn1:"optional,explicit,tag:1"`
	AuthInfo    []byte      `asn1:"optional,explicit,tag:2"`
	PubKeyAuth  []byte      `asn1:"optional,explicit,tag:3"`
	ErrorCode   int         `asn1:"optional,explicit,tag:4"`
	ClientNonce []byte      `asn1:"optional,explicit,tag:5"`
}

// Request is a decoded CredSSP message.
type Request struct {
	Version     int
	NegoToken   []byte
	AuthInfo    []byte
	PubKeyAuth  []byte
	ErrorCode   int
	ClientNonce []byte
}

// Encode serialises a CredSSP message as DER.
func (r Request) Encode() ([]byte, error) {
	req := tsRequest{
		Version:     r.Version,
		AuthInfo:    r.AuthInfo,
		PubKeyAuth:  r.PubKeyAuth,
		ErrorCode:   r.ErrorCode,
		ClientNonce: r.ClientNonce,
	}
	if len(r.NegoToken) > 0 {
		req.NegoTokens = []negoToken{{Token: r.NegoToken}}
	}
	return asn1.Marshal(req)
}

// ParseRequest decodes a CredSSP message.
//
// Trailing bytes are refused rather than ignored. A DER structure followed by
// anything else is not the structure it claims to be, and accepting the prefix
// means the peer chooses which part of its own message gets interpreted.
func ParseRequest(b []byte) (Request, error) {
	var req tsRequest
	rest, err := asn1.Unmarshal(b, &req)
	if err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if len(rest) != 0 {
		return Request{}, fmt.Errorf("%w: %d trailing bytes after the TSRequest",
			ErrMalformed, len(rest))
	}

	out := Request{
		Version:     req.Version,
		AuthInfo:    req.AuthInfo,
		PubKeyAuth:  req.PubKeyAuth,
		ErrorCode:   req.ErrorCode,
		ClientNonce: req.ClientNonce,
	}
	if len(req.NegoTokens) > 0 {
		out.NegoToken = req.NegoTokens[0].Token
	}
	return out, nil
}

/* ── Credentials ─────────────────────────────────────────────────────────── */

// tsPasswordCreds is MS-CSSP 2.2.1.2.1.
type tsPasswordCreds struct {
	DomainName []byte `asn1:"explicit,tag:0"`
	UserName   []byte `asn1:"explicit,tag:1"`
	Password   []byte `asn1:"explicit,tag:2"`
}

// tsCredentials is MS-CSSP 2.2.1.2.
type tsCredentials struct {
	CredType    int    `asn1:"explicit,tag:0"`
	Credentials []byte `asn1:"explicit,tag:1"`
}

// EncodeCredentials serialises the credentials Argus delegates to the host.
//
// This is the actual secret, and it goes on the wire encrypted under the
// negotiated session key — never in the clear, and never before the public key
// binding has confirmed which channel it is being sent over.
func EncodeCredentials(c Credentials) ([]byte, error) {
	inner, err := asn1.Marshal(tsPasswordCreds{
		DomainName: utf16le(c.Domain),
		UserName:   utf16le(c.User),
		Password:   utf16le(c.Password),
	})
	if err != nil {
		return nil, err
	}
	// credType 1 is a password. Smart card credentials are a different type
	// Argus does not delegate.
	return asn1.Marshal(tsCredentials{CredType: 1, Credentials: inner})
}

/* ── Public key binding ──────────────────────────────────────────────────── */

// ErrPubKeyMismatch reports a server whose public key binding does not match
// the channel it was received on.
//
// This is the whole point of the step: it means the TLS connection the server
// authenticated over is not the one carrying this exchange. Whatever the cause,
// the credentials must not be sent.
var ErrPubKeyMismatch = errors.New("the server's public key binding does not match this connection")

// NewNonce returns the 32-byte client nonce for version 5 and above.
func NewNonce() ([]byte, error) {
	n := make([]byte, 32)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("generate client nonce: %w", err)
	}
	return n, nil
}

// PubKeyAuth computes the client's public key binding.
//
// The server's TLS public key is hashed together with a magic string and the
// client nonce, then sealed. The server recomputes it, so a value that matches
// proves the peer holds both the session key and the private half of the
// certificate on this connection — which is what makes an interposed proxy
// visible.
//
// Argus terminates TLS on both sides, so it computes this against the target's
// key on the target side and against its own key on the client side. That is
// not a defeat of the mechanism but a statement of where trust sits: a client
// is protected because it deliberately connected to Argus and verified Argus's
// certificate, exactly as on the SSH side.
func PubKeyAuth(sealer *Sealer, publicKey, nonce []byte) []byte {
	return sealer.Seal(bindingHash(clientToServerBinding, nonce, publicKey))
}

// VerifyPubKeyAuth checks the server's answer.
func VerifyPubKeyAuth(sealer *Sealer, response, publicKey, nonce []byte) error {
	got, err := sealer.Unseal(response)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPubKeyMismatch, err)
	}
	want := bindingHash(serverToClientBinding, nonce, publicKey)
	if !constantTimeEqual(got, want) {
		return ErrPubKeyMismatch
	}
	return nil
}

// VerifyLegacyPubKeyAuth checks a pre-version-5 server's answer.
//
// Older servers return the public key itself with the first byte incremented,
// rather than a hash bound to a nonce. Supported because Windows Server 2008
// and 2012 hosts are still in service, and refusing them outright would push
// operators towards leaving RDP unbrokered — which is worse than brokering it
// with a weaker binding. The weaker check is recorded on the session so an
// auditor can see which was used.
func VerifyLegacyPubKeyAuth(sealer *Sealer, response, publicKey []byte) error {
	got, err := sealer.Unseal(response)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPubKeyMismatch, err)
	}
	if len(got) != len(publicKey) || len(got) == 0 {
		return ErrPubKeyMismatch
	}
	want := append([]byte{}, publicKey...)
	want[0]++
	if !constantTimeEqual(got, want) {
		return ErrPubKeyMismatch
	}
	return nil
}

// bindingHash is SHA-256(magic || nonce || publicKey).
func bindingHash(magic string, nonce, publicKey []byte) []byte {
	h := sha256.New()
	h.Write([]byte(magic))
	h.Write(nonce)
	h.Write(publicKey)
	return h.Sum(nil)
}

// constantTimeEqual compares without leaking where two values first differ.
func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
