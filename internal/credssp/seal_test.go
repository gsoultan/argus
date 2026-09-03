package credssp

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func newPair(t *testing.T, flags uint32) (client, server *Sealer) {
	t.Helper()
	key := mustHex(t, "55555555555555555555555555555555")
	c, err := NewSealer(key, flags, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSealer(key, flags, false)
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestSealRoundTrip(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)

	for i, msg := range [][]byte{
		[]byte("first message"),
		[]byte("second message, longer than the first"),
		{},
	} {
		sealed := client.Seal(msg)
		if len(msg) > 0 && bytes.Contains(sealed, msg) {
			t.Errorf("message %d appears in the output in the clear", i)
		}
		got, err := server.Unseal(sealed)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, msg) {
			t.Errorf("message %d round-tripped as %q, want %q", i, got, msg)
		}
	}
}

// The RC4 handles are one continuous keystream across the exchange. A fresh
// cipher per message would encrypt identical plaintexts identically, which is
// the classic way a stream cipher leaks.
func TestKeystreamAdvancesAcrossMessages(t *testing.T) {
	client, _ := newPair(t, NegotiateKeyExchange)

	first := client.Seal([]byte("the same plaintext"))
	second := client.Seal([]byte("the same plaintext"))

	if bytes.Equal(first[16:], second[16:]) {
		t.Fatal("the same plaintext sealed to the same ciphertext twice; " +
			"the keystream is being reused")
	}
	if bytes.Equal(first[4:12], second[4:12]) {
		t.Error("the same checksum was produced twice")
	}
}

// Sequence numbers are part of every signature, so the two sides must advance
// in lockstep. A message accepted out of order would let a replayed one land at
// a position it was never sealed for.
func TestOutOfOrderMessagesAreRefused(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)

	first := client.Seal([]byte("one"))
	second := client.Seal([]byte("two"))

	if _, err := server.Unseal(second); err == nil {
		t.Error("a message from the wrong sequence position was accepted")
	}
	// And the first no longer verifies either, because the receiving keystream
	// has already advanced past it.
	if _, err := server.Unseal(first); err == nil {
		t.Error("the exchange recovered from a desynchronised keystream")
	}
}

func TestTamperedCiphertextIsRefused(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	sealed := client.Seal([]byte("transfer 100 to alice"))

	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := server.Unseal(tampered); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

func TestTamperedSignatureIsRefused(t *testing.T) {
	client, server := newPair(t, NegotiateKeyExchange)
	sealed := client.Seal([]byte("message"))
	sealed[5] ^= 0xFF

	if _, err := server.Unseal(sealed); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

// Sealing with the client keys and unsealing with the client keys must fail:
// the four directional keys exist precisely so each direction is distinct.
func TestDirectionalKeysAreNotInterchangeable(t *testing.T) {
	key := mustHex(t, "55555555555555555555555555555555")
	a, _ := NewSealer(key, NegotiateKeyExchange, true)
	b, _ := NewSealer(key, NegotiateKeyExchange, true) // also a client

	if _, err := b.Unseal(a.Seal([]byte("message"))); err == nil {
		t.Error("a client unsealed another client's message; the send and " +
			"receive keys are the same")
	}
}

func TestSealerRejectsAWrongSizedKey(t *testing.T) {
	if _, err := NewSealer(make([]byte, 8), 0, true); err == nil {
		t.Error("an 8-byte session key was accepted")
	}
}

func TestUnsealRejectsAShortMessage(t *testing.T) {
	_, server := newPair(t, NegotiateKeyExchange)
	if _, err := server.Unseal(make([]byte, 8)); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// Without key exchange negotiated the checksum is not itself RC4'd. Both sides
// must agree, or every message fails to verify.
func TestWithoutKeyExchangeTheChecksumIsPlain(t *testing.T) {
	client, server := newPair(t, 0)
	got, err := server.Unseal(client.Seal([]byte("message")))
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if string(got) != "message" {
		t.Errorf("round trip gave %q", got)
	}
}

func TestDerivedKeysAreDirectional(t *testing.T) {
	key := mustHex(t, "55555555555555555555555555555555")
	seen := map[string]string{}
	for name, magic := range map[string]string{
		"client seal": clientSealMagic,
		"server seal": serverSealMagic,
		"client sign": clientSignMagic,
		"server sign": serverSignMagic,
	} {
		h := hex.EncodeToString(deriveKey(key, magic))
		if other, dup := seen[h]; dup {
			t.Errorf("%s and %s derived the same key", name, other)
		}
		seen[h] = name
	}
}
