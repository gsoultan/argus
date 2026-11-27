package secretwire

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

func TestSealOpensOnlyForItsRecipientAndContext(t *testing.T) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	sealed, err := SealTo(priv.PublicKey(), []byte("hunter2"), ReadAAD("prod/db", 3, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenSealed(priv, sealed, ReadAAD("prod/db", 3, "password")); err != nil || string(got) != "hunter2" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := OpenSealed(other, sealed, ReadAAD("prod/db", 3, "password")); err == nil {
		t.Fatal("another key opened it")
	}
	// The same ciphertext passed off as another version or field must not open.
	if _, err := OpenSealed(priv, sealed, ReadAAD("prod/db", 2, "password")); err == nil {
		t.Fatal("opened as a different version")
	}
	if _, err := OpenSealed(priv, sealed, ReadAAD("prod/db", 3, "username")); err == nil {
		t.Fatal("opened as a different field")
	}
	// Truncated or with another format version.
	if _, err := OpenSealed(priv, sealed[:20], ReadAAD("prod/db", 3, "password")); err == nil {
		t.Fatal("opened a truncated value")
	}
	bad := append([]byte{2}, sealed[1:]...)
	if _, err := OpenSealed(priv, bad, ReadAAD("prod/db", 3, "password")); err == nil {
		t.Fatal("opened a value of another format version")
	}
}

func TestParseRecipientRefusesWhatIsNotAKey(t *testing.T) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := ParseRecipient(EncodeKey(priv.PublicKey().Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", "abc", EncodeKey(make([]byte, 31)), EncodeKey(make([]byte, PQPublicKeySize-1)), "!!!!"} {
		if _, err := ParseRecipient(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
