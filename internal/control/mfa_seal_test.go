package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// storedMFA reads the columns as the database holds them.
func storedMFA(t *testing.T, s *Store, email string) (secret, pending string) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `
		SELECT coalesce(totp_secret, ''), coalesce(totp_pending_secret, '')
		  FROM users WHERE lower(email) = lower($1)`, email).Scan(&secret, &pending); err != nil {
		t.Fatal(err)
	}
	return secret, pending
}

// enrol begins and confirms an enrolment and returns the secret.
func enrol(t *testing.T, s *Store, email string) string {
	t.Helper()
	ctx := context.Background()
	secret, err := s.BeginMFAEnrolment(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	if _, err := s.ConfirmMFAEnrolment(ctx, email, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return secret
}

// signsInWith reports whether the next code from secret is accepted.
func signsInWith(t *testing.T, s *Store, email, secret string) bool {
	t.Helper()
	ctx := context.Background()
	a, err := s.Account(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	// The next time step: the enrolment spent this one.
	code, _ := auth.TOTPCode(secret, time.Now().Add(30*time.Second))
	ok, err := s.ConsumeTOTP(ctx, a, code, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func sealedStore(t *testing.T, key []byte) *Store {
	t.Helper()
	s := testStore(t)
	s.SetMFAKey(key)
	if _, err := s.StartSealingMFA(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthenticatorSecretsAreSealedAtRest(t *testing.T) {
	s := sealedStore(t, MFAKeyFromSecret("one-signing-secret-of-sufficient-length"))
	email, _ := newAccount(t, s, "admin")

	secret, err := s.BeginMFAEnrolment(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	_, pending := storedMFA(t, s, email)
	if !strings.HasPrefix(pending, mfaSealPrefix) || strings.Contains(pending, secret) {
		t.Fatalf("a pending enrolment is stored as %q, not sealed", pending)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	if _, err := s.ConfirmMFAEnrolment(context.Background(), email, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	stored, _ := storedMFA(t, s, email)
	if !strings.HasPrefix(stored, mfaSealPrefix) || strings.Contains(stored, secret) {
		t.Fatalf("the authenticator secret is stored as %q, not sealed", stored)
	}
	if !signsInWith(t, s, email, secret) {
		t.Error("a code from the sealed secret was refused")
	}
}

// The upgrade: secrets stored by a version before sealing keep working, are
// sealed in place, and keep working after. Nobody enrols again.
func TestExistingAuthenticatorSecretsAreSealedInPlace(t *testing.T) {
	s := testStore(t) // a version before sealing: no key
	email, _ := newAccount(t, s, "admin")
	secret := enrol(t, s, email)
	if stored, _ := storedMFA(t, s, email); stored != secret {
		t.Fatalf("set-up: expected the secret unsealed, got %q", stored)
	}

	s.SetMFAKey(MFAKeyFromSecret("one-signing-secret-of-sufficient-length"))
	if !signsInWith(t, s, email, secret) {
		t.Fatal("an unsealed secret must still verify once a key is set")
	}
	n, err := s.StartSealingMFA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("sealed %d secrets, want at least this account's", n)
	}
	stored, _ := storedMFA(t, s, email)
	if !strings.HasPrefix(stored, mfaSealPrefix) {
		t.Fatalf("the stored secret was not sealed in place: %q", stored)
	}
	// Spend a later step than signsInWith did above.
	a, _ := s.Account(context.Background(), email)
	code, _ := auth.TOTPCode(secret, time.Now().Add(60*time.Second))
	if ok, _ := s.ConsumeTOTP(context.Background(), a, code, time.Now().Add(30*time.Second)); !ok {
		t.Error("the authenticator stopped working when its secret was sealed")
	}
	// Running it again changes nothing.
	again, _ := storedMFA(t, s, email)
	if _, err := s.StartSealingMFA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after, _ := storedMFA(t, s, email); after != again {
		t.Error("sealing again rewrote a sealed secret")
	}
}

// Until sealing starts, a new version writes secrets the version before it
// can read, so an update rolled back at once loses nobody's second factor.
func TestSecretsStayReadableByTheVersionBeforeUntilSealingStarts(t *testing.T) {
	s := testStore(t)
	s.SetMFAKey(MFAKeyFromSecret("one-signing-secret-of-sufficient-length"))
	email, _ := newAccount(t, s, "admin")
	secret := enrol(t, s, email)
	if stored, _ := storedMFA(t, s, email); stored != secret {
		t.Errorf("before sealing starts, a new secret must be stored as before, got %q", stored)
	}
}

func TestASecretSealedUnderAnotherKeyIsRefused(t *testing.T) {
	s := sealedStore(t, MFAKeyFromSecret("one-signing-secret-of-sufficient-length"))
	email, _ := newAccount(t, s, "admin")
	secret := enrol(t, s, email)

	other := testStore(t)
	other.SetMFAKey(MFAKeyFromSecret("another-signing-secret-of-sufficient-length"))
	if signsInWith(t, other, email, secret) {
		t.Error("a secret sealed under one key verified under another")
	}
	noKey := testStore(t)
	if signsInWith(t, noKey, email, secret) {
		t.Error("a sealed secret verified on a control plane without the key")
	}
}

// A rekey moves the secrets to the new key, so they keep working after the
// signing secret changes; moving them back undoes it.
func TestRekeyMovesAuthenticatorSecrets(t *testing.T) {
	oldKey := MFAKeyFromSecret("one-signing-secret-of-sufficient-length")
	newKey := MFAKeyFromSecret("another-signing-secret-of-sufficient-length")
	s := sealedStore(t, oldKey)
	email, _ := newAccount(t, s, "admin")
	secret := enrol(t, s, email)

	if _, err := s.ResealMFASecrets(context.Background(), newKey); err != nil {
		t.Fatal(err)
	}
	moved := testStore(t)
	moved.SetMFAKey(newKey)
	if !signsInWith(t, moved, email, secret) {
		t.Fatal("after a rekey the authenticator was refused under the new key")
	}
	// Already moved: a second run leaves it.
	before, _ := storedMFA(t, s, email)
	if _, err := s.ResealMFASecrets(context.Background(), newKey); err != nil {
		t.Fatal(err)
	}
	if after, _ := storedMFA(t, s, email); after != before {
		t.Error("moving again rewrote a secret already under the new key")
	}
	// And back, as a failed rekey does.
	if _, err := moved.ResealMFASecrets(context.Background(), oldKey); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Account(context.Background(), email)
	if a.TOTPSecret != secret {
		t.Error("moving the secrets back did not restore them under the old key")
	}
}

func TestSealedSecretsOpenOnlyUnderTheirKey(t *testing.T) {
	k1, k2 := MFAKeyFromSecret("a"), MFAKeyFromSecret("b")
	sealed, err := sealMFAWith(k1, "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := openMFAWith(k1, sealed); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Errorf("open under its key: %q, %v", got, err)
	}
	for name, key := range map[string][]byte{"another key": k2, "no key": nil} {
		if _, err := openMFAWith(key, sealed); err == nil {
			t.Errorf("opened under %s", name)
		}
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := openMFAWith(k1, tampered); err == nil {
		t.Error("a changed sealed value opened")
	}
	if got, _ := openMFAWith(k1, "JBSWY3DPEHPK3PXP"); got != "JBSWY3DPEHPK3PXP" {
		t.Error("a secret from before sealing must come back as it is")
	}
	if MFAKeyFromSecret("") != nil {
		t.Error("no signing secret must mean no key")
	}
}
