package secretwire

import (
	"bytes"
	"encoding/base64"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

func TestPostQuantumSealOpensOnlyForItsRecipientAndContext(t *testing.T) {
	priv, err := NewPQKey()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewPQKey()
	if n := len(priv.PublicKey().Bytes()); n != PQPublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", n, PQPublicKeySize)
	}
	aad := ReadAAD("prod/db", 3, "password")
	sealed, err := SealPQ(priv.PublicKey(), []byte("hunter2"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if SealVersion(sealed) != 2 || len(sealed) != 1+pqEncSize+len("hunter2")+16 {
		t.Fatalf("version %d, %d bytes", SealVersion(sealed), len(sealed))
	}
	if got, err := OpenPQ(priv, sealed, aad); err != nil || string(got) != "hunter2" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := OpenPQ(other, sealed, aad); err == nil {
		t.Fatal("another key opened it")
	}
	for _, wrong := range [][]byte{ReadAAD("prod/db", 2, "password"), ReadAAD("prod/db", 3, "username"), ReadAAD("prod/other", 3, "password")} {
		if _, err := OpenPQ(priv, sealed, wrong); err == nil {
			t.Fatalf("opened under %q", wrong)
		}
	}
	// Any flipped bit, in the encapsulated key or the ciphertext, is refused.
	for _, i := range []int{1, 1 + pqEncSize - 1, 1 + pqEncSize, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[i] ^= 1
		if _, err := OpenPQ(priv, bad, aad); err == nil {
			t.Fatalf("opened with byte %d flipped", i)
		}
	}
	if _, err := OpenPQ(priv, sealed[:40], aad); err == nil {
		t.Fatal("opened a truncated value")
	}
}

// A caller that asked for a post-quantum seal must not accept a v1 answer,
// or an attacker could downgrade the reply.
func TestPostQuantumRecipientNeverAcceptsAV1Answer(t *testing.T) {
	pq, _ := NewPQKey()
	x, _ := ecdh.X25519().GenerateKey(rand.Reader)
	v1, _ := SealTo(x.PublicKey(), []byte("hunter2"), WriteAAD("prod/db"))
	if _, err := OpenPQ(pq, v1, WriteAAD("prod/db")); err == nil {
		t.Fatal("OpenPQ opened a v1 value")
	}
	r, err := ParseRecipient(EncodeKey(pq.PublicKey().Bytes()))
	if err != nil || !r.PostQuantum() {
		t.Fatalf("hybrid key parsed as %T, %v", r, err)
	}
	sealed, _ := r.Seal([]byte("hunter2"), WriteAAD("prod/db"))
	if SealVersion(sealed) != 2 {
		t.Fatal("a hybrid recipient got a v1 seal")
	}
	r1, _ := ParseRecipient(EncodeKey(x.PublicKey().Bytes()))
	if r1.PostQuantum() {
		t.Fatal("an X25519 key is not post-quantum")
	}
}

func TestOpenEitherDispatchesByVersion(t *testing.T) {
	pq, _ := NewPQKey()
	x, _ := ecdh.X25519().GenerateKey(rand.Reader)
	aad := WriteAAD("prod/db")
	v1, _ := SealTo(x.PublicKey(), []byte("one"), aad)
	v2, _ := SealPQ(pq.PublicKey(), []byte("two"), aad)
	if got, err := OpenEither(x, pq, v1, aad); err != nil || string(got) != "one" {
		t.Fatalf("v1: %q %v", got, err)
	}
	if got, err := OpenEither(x, pq, v2, aad); err != nil || string(got) != "two" {
		t.Fatalf("v2: %q %v", got, err)
	}
	if _, err := OpenEither(nil, pq, v1, aad); err == nil {
		t.Fatal("v1 opened with no X25519 key")
	}
	if _, err := OpenEither(x, pq, append([]byte{9}, v2[1:]...), aad); err == nil {
		t.Fatal("an unknown version opened")
	}
}

func TestPQPrivateKeyRoundTrips(t *testing.T) {
	k, _ := NewPQKey()
	raw, err := k.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	back, err := PQPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := SealPQ(k.PublicKey(), []byte("x"), nil)
	if _, err := OpenPQ(back, sealed, nil); err != nil {
		t.Fatal(err)
	}
}

// Sealed in the console by web/src/mf-secrets/seal.ts (HPKE with
// @noble/post-quantum's X-Wing) to the key in seal-pq.test.ts, as a write of
// prod/db. Go must open what the browser seals, byte for byte.
func TestBrowserSealOpensInGo(t *testing.T) {
	const sk = "QIpWIiUlm4P-n8MnxS1mUBmvcLwZ4NA71ZOe07Fb5d0"
	const sealed = "AoxfInuSZNZ4aXYGiTJOXz3xP5ApIhC8dm1Hz6QyZvNmNrA2xpOA5pK0weL/qFN+hub38qYtdXnKSE2XRDb7CybbmwaSI8KlkEzp96B50UwOYu6IaeNzm0oElyd+xhxE0FdKAU/Te669A4fBWRcQ2/C2sZOmM2Z7QLrhar4SPJXpqvpOvq5jt8oHsE+j0v7T836DkzaTSFzxRJ1KSjuhOoGlPcoyF+H0Imnqbrs+S7JYAjHYAgCK7t/IlTSFq2VcLQh0cG98VkqjTB5gNBR++fe0koiilBexJa6LUn7BlXEthYQFM+uxGUFGHw6lpm5ig4VKQXC/KC4/WHh8w/jPTFxRF1bzxxWO3yKyZc9M/lXjQaHlN96yVMRLhnvGK8Wr8fjZjIHmYwiOBcSOkwYpj5Qao2Q9xjmEhJJs5MBWmPmfaxzhzGqpCMBruv3w+H7W7r3wNkSUZUid4f0mwlQo+7zNmGZk0H0J3RcZRdalWhghGlrtsg3ppXXj5pTK5qfAlKNBpaM8Bofu1hTmTmBAfurZoNdxdPctfWbmERXapCVzVfKxOLWj+tuMatciOV4GmFEYg6Y82siXCraoqwpFp80e203aBX1n8mHnwWMp8RpIHZLbx3hl5mhsPP4Teja9aMo78CPeImslLtNahqHppGeBFayEklgBuMgF/f7af16wtw+NmMwRzjGi5qxBe3B6uma2YwL9zkVU3ipnWfQWM0JriAQ5lxQv20/e+PzhzFna6XePiJ0iMSaZLGG/KykHrCQy31bB/vLPhbrYqkI9gcvFvPijyNngZhlHR1nJB4spwN/3Hi9UVKcFKFyLtXDgs4PdrDmHFVT+CYN1aU8rmsxXxKTPWUlA12axjyWgLPpdVM7k29kqXPc7nM9/DCQhx/g6SBe0SLWApa/cUpsdonYqwwmZI0tDhL6DxqVQDQlYjCt31NPZPdg1VCUO6PH+b1mVoJ8iP81xq9FNVqZ1d1LkcLLkexzQJ7YFYAliwwRKa9G/BaNPDHpN24WpmRWy8oLbM2Htglok1taio5V8e+xNDMRclewA1CIyzlsna6Y7aWnXhmoVQPDuwcLQk8r9815ym2HBuj6X+mIcihE4J3cB2wQBMnulLQMH89dqOyzM0PioQ6eFc+EQDa0eXFckeMrKRF2HohimXLVwlLogHSHnUkmbK/sO1cJ38P8cVaVH5+snCWatytsDMJLFlXP1ZwAYW8uzDf2+bhlTTKQ2DsBXXNhcijc+IgSpJCe67ma04ofLks0Hx1AqncGmtVJZABsIQ4VSTlByGoyrEJpwwYbJjyQQ0UDNQ+qyz4wbLrcHCVMZ4y1OcMVFXKN5F8AONgyvZJeWg50gb7JewR7lTuSNcDNi+x9aKaBn7adPVhOwXVocbbz2F/RzFAWD4nbguIQzKsBTKP7wigSLGV744yRHKkDB9doJJyrTPMcZTxHI3yov3S4dwn7KyQPIIt+Ofqe/5g2BurTdlQ3gP5ak4249VTg/JRifIO7SHtj6evZDdZV3Y5mfO46qRStDrm77SQ=="
	raw, _ := base64.RawURLEncoding.DecodeString(sk)
	k, err := PQPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := base64.StdEncoding.DecodeString(sealed)
	got, err := OpenPQ(k, ct, WriteAAD("prod/db"))
	if err != nil || string(got) != "from the console" {
		t.Fatalf("got %q, %v", got, err)
	}
}
