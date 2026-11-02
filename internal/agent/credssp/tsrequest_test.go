package credssp

import (
	"bytes"
	"encoding/asn1"
	"errors"
	"strings"
	"testing"
)

func TestTSRequestRoundTrip(t *testing.T) {
	in := Request{
		Version:     Version,
		NegoToken:   []byte("NTLMSSP\x00 token bytes"),
		PubKeyAuth:  []byte{1, 2, 3, 4},
		ClientNonce: bytes.Repeat([]byte{0xAB}, 32),
	}
	der, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, err := ParseRequest(der)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if out.Version != in.Version {
		t.Errorf("Version = %d", out.Version)
	}
	if !bytes.Equal(out.NegoToken, in.NegoToken) {
		t.Errorf("NegoToken = %q", out.NegoToken)
	}
	if !bytes.Equal(out.PubKeyAuth, in.PubKeyAuth) {
		t.Errorf("PubKeyAuth = %x", out.PubKeyAuth)
	}
	if !bytes.Equal(out.ClientNonce, in.ClientNonce) {
		t.Errorf("ClientNonce = %x", out.ClientNonce)
	}
}

func TestTSRequestOmitsAbsentFields(t *testing.T) {
	der, err := Request{Version: Version, NegoToken: []byte("token")}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if out.AuthInfo != nil || out.PubKeyAuth != nil || out.ClientNonce != nil {
		t.Errorf("absent fields decoded as present: %+v", out)
	}
}

// A DER structure followed by anything else is not the structure it claims to
// be. Accepting the prefix would let the peer choose which part of its own
// message gets interpreted.
func TestTSRequestRefusesTrailingBytes(t *testing.T) {
	der, err := Request{Version: Version, NegoToken: []byte("token")}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseRequest(append(der, 0xFF, 0xFF))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

// These bytes come from the target over the network before anything has been
// authenticated, so a malformed structure must be an error rather than a panic.
func TestTSRequestRefusesGarbage(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":           {},
		"truncated":       {0x30, 0x80},
		"not a sequence":  {0x02, 0x01, 0x06},
		"length overflow": {0x30, 0x7F, 0x01},
		"random":          []byte("this is not DER at all"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRequest(in); err == nil {
				t.Error("garbage parsed without error")
			}
		})
	}
}

func FuzzParseRequest(f *testing.F) {
	der, _ := Request{Version: Version, NegoToken: []byte("token"),
		ClientNonce: bytes.Repeat([]byte{1}, 32)}.Encode()
	f.Add(der)
	f.Add([]byte{0x30, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseRequest(b)
	})
}

/* ── Credentials ─────────────────────────────────────────────────────────── */

func TestEncodeCredentials(t *testing.T) {
	der, err := EncodeCredentials(Credentials{
		Domain: "CORP", User: "ops", Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("EncodeCredentials: %v", err)
	}

	var outer tsCredentials
	if _, err := asn1.Unmarshal(der, &outer); err != nil {
		t.Fatalf("outer structure: %v", err)
	}
	if outer.CredType != 1 {
		t.Errorf("CredType = %d, want 1 (password)", outer.CredType)
	}

	var inner tsPasswordCreds
	if _, err := asn1.Unmarshal(outer.Credentials, &inner); err != nil {
		t.Fatalf("inner structure: %v", err)
	}
	if decodeUTF16(inner.UserName) != "ops" {
		t.Errorf("UserName = %q", decodeUTF16(inner.UserName))
	}
	if decodeUTF16(inner.Password) != "hunter2" {
		t.Errorf("Password = %q", decodeUTF16(inner.Password))
	}
	if decodeUTF16(inner.DomainName) != "CORP" {
		t.Errorf("DomainName = %q", decodeUTF16(inner.DomainName))
	}
}

/* ── Public key binding ──────────────────────────────────────────────────── */

// The step that makes an interposed proxy visible. A server that answers with a
// binding over a different public key is on a different channel from the one
// carrying this exchange, and the credentials must not follow.
func TestPubKeyAuthRoundTrip(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	pubKey := []byte("the target's TLS public key")
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}

	// Client sends its binding; the server would verify it the same way.
	_ = PubKeyAuth(client, pubKey, nonce)

	// Server answers with its own direction's magic string.
	answer := server.Seal(bindingHash(serverToClientBinding, nonce, pubKey))
	if err := VerifyPubKeyAuth(client, answer, pubKey, nonce); err != nil {
		t.Fatalf("VerifyPubKeyAuth on a correct answer: %v", err)
	}
}

func TestPubKeyAuthRefusesAWrongKey(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	nonce, _ := NewNonce()

	// The server binds to a different key from the one on this connection —
	// which is what an interposed proxy looks like.
	answer := server.Seal(bindingHash(serverToClientBinding, nonce, []byte("a different key")))

	err := VerifyPubKeyAuth(client, answer, []byte("the real key"), nonce)
	if !errors.Is(err, ErrPubKeyMismatch) {
		t.Errorf("err = %v, want ErrPubKeyMismatch", err)
	}
}

// The nonce is what stops a captured exchange being replayed against a
// different channel, so a mismatched one must fail.
func TestPubKeyAuthRefusesAWrongNonce(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	pubKey := []byte("key")
	theirs, _ := NewNonce()
	ours, _ := NewNonce()

	answer := server.Seal(bindingHash(serverToClientBinding, theirs, pubKey))
	if err := VerifyPubKeyAuth(client, answer, pubKey, ours); !errors.Is(err, ErrPubKeyMismatch) {
		t.Errorf("err = %v, want ErrPubKeyMismatch", err)
	}
}

// Using the client's own magic string for the server's answer would let a
// captured client message be replayed straight back as a server response.
func TestPubKeyAuthDirectionsAreDistinct(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	pubKey := []byte("key")
	nonce, _ := NewNonce()

	wrongDirection := server.Seal(bindingHash(clientToServerBinding, nonce, pubKey))
	if err := VerifyPubKeyAuth(client, wrongDirection, pubKey, nonce); !errors.Is(err, ErrPubKeyMismatch) {
		t.Error("a client-direction binding was accepted as a server answer")
	}
}

func TestLegacyPubKeyAuth(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	pubKey := []byte{0x30, 0x82, 0x01, 0x0A}

	incremented := append([]byte{}, pubKey...)
	incremented[0]++
	answer := server.Seal(incremented)
	if err := VerifyLegacyPubKeyAuth(client, answer, pubKey); err != nil {
		t.Fatalf("a correct legacy answer was refused: %v", err)
	}
}

func TestLegacyPubKeyAuthRefusesTheUnincrementedKey(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	pubKey := []byte{0x30, 0x82, 0x01, 0x0A}

	// Echoing the key back unchanged is what a naive proxy would do.
	answer := server.Seal(pubKey)
	if err := VerifyLegacyPubKeyAuth(client, answer, pubKey); !errors.Is(err, ErrPubKeyMismatch) {
		t.Errorf("err = %v, want ErrPubKeyMismatch", err)
	}
}

func TestNonceIsLongAndUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		n, err := NewNonce()
		if err != nil {
			t.Fatal(err)
		}
		if len(n) != 32 {
			t.Fatalf("nonce is %d bytes, want 32", len(n))
		}
		if seen[string(n)] {
			t.Fatal("the same nonce was generated twice")
		}
		seen[string(n)] = true
	}
}

func TestVersionEnablesTheNonceBinding(t *testing.T) {
	// Below 5 the binding is computed over the bare public key, which a
	// captured exchange can replay against another channel.
	if Version < 5 {
		t.Errorf("Version = %d, which predates the nonce-based binding", Version)
	}
}
