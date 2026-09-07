package auth

import (
	"errors"
	"strings"
	"testing"
)

const good = "correct horse battery staple"

func TestHashAndVerifyRoundTrip(t *testing.T) {
	h, err := HashPassword(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(h, good); err != nil {
		t.Errorf("the password that was hashed must verify: %v", err)
	}
	if err := VerifyPassword(h, good+"x"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("a wrong password must not verify: %v", err)
	}
}

// The salt is what stops one cracked password revealing every account that
// shares it, so two hashes of the same input must differ.
func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword(good)
	b, _ := HashPassword(good)
	if a == b {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	if err := VerifyPassword(a, good); err != nil {
		t.Error(err)
	}
	if err := VerifyPassword(b, good); err != nil {
		t.Error(err)
	}
}

func TestHashRecordsItsParameters(t *testing.T) {
	h, _ := HashPassword(good)
	// Parameters live in the hash so raising them later does not invalidate
	// every stored password.
	for _, want := range []string{"$argon2id$", "v=19", "m=19456", "t=2", "p=1"} {
		if !strings.Contains(h, want) {
			t.Errorf("hash does not record %q: %s", want, h)
		}
	}
}

func TestShortPasswordsAreRefused(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("a five-character password was accepted")
	}
	// The boundary, stated exactly.
	if _, err := HashPassword(strings.Repeat("a", MinPasswordLength-1)); err == nil {
		t.Error("one character under the minimum was accepted")
	}
	if _, err := HashPassword(strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Errorf("exactly the minimum was refused: %v", err)
	}
}

// A stored hash that has been corrupted, truncated or was never argon2 must
// fail closed rather than panic or accidentally match.
func TestMalformedHashesNeverVerify(t *testing.T) {
	valid, _ := HashPassword(good)
	for name, h := range map[string]string{
		"empty":            "",
		"not a hash":       "hunter2",
		"bcrypt":           "$2a$10$abcdefghijklmnopqrstuv",
		"wrong algorithm":  "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"truncated":        valid[:len(valid)-10],
		"bad base64 salt":  "$argon2id$v=19$m=19456,t=2,p=1$!!!!$aGFzaA",
		"missing sections": "$argon2id$v=19$m=19456,t=2,p=1",
	} {
		if err := VerifyPassword(h, good); err == nil {
			t.Errorf("%s: a malformed hash verified", name)
		}
	}
}

// Every failure must be indistinguishable, or the error becomes an oracle for
// which accounts exist and what state they are in.
func TestFailuresAreAllTheSameError(t *testing.T) {
	valid, _ := HashPassword(good)
	for _, h := range []string{valid, "", "garbage", "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		err := VerifyPassword(h, "definitely wrong")
		if err == nil {
			t.Fatalf("expected a failure for %q", h)
		}
		if !errors.Is(err, ErrPasswordMismatch) {
			t.Errorf("failure is not ErrPasswordMismatch, so callers can tell cases apart: %v", err)
		}
	}
}
