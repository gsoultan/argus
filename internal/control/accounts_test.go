package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

func newAccount(t *testing.T, s *Store, role string) (email, password string) {
	t.Helper()
	email = unique("user") + "@northwind.id"
	password = "correct horse battery staple"
	if _, err := s.CreateAccount(context.Background(), email, "Test User", role, password); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return email, password
}

func TestAuthenticateAcceptsTheRightPassword(t *testing.T) {
	s := testStore(t)
	email, password := newAccount(t, s, "admin")

	a, err := s.Authenticate(context.Background(), email, password)
	if err != nil {
		t.Fatalf("the correct password was refused: %v", err)
	}
	if a.Role != "admin" || !strings.EqualFold(a.Email, email) {
		t.Errorf("wrong account returned: %+v", a)
	}
	// Addresses are not case sensitive; people type them however they like.
	if _, err := s.Authenticate(context.Background(), strings.ToUpper(email), password); err != nil {
		t.Errorf("an upper-case address was refused: %v", err)
	}
}

// Every failure must be the same error, or it enumerates the user list.
func TestAuthenticateRefusalsAreIndistinguishable(t *testing.T) {
	s := testStore(t)
	email, password := newAccount(t, s, "operator")
	ctx := context.Background()

	for name, attempt := range map[string][2]string{
		"wrong password":  {email, "not the password"},
		"unknown address": {unique("nobody") + "@northwind.id", password},
		"empty password":  {email, ""},
	} {
		_, err := s.Authenticate(ctx, attempt[0], attempt[1])
		if !errors.Is(err, ErrNoAccount) {
			t.Errorf("%s: got %v, want ErrNoAccount", name, err)
		}
	}
}

// A disabled account keeps its history but must not be able to sign in.
func TestDisabledAccountsCannotAuthenticate(t *testing.T) {
	s := testStore(t)
	email, password := newAccount(t, s, "operator")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET disabled_at = now() WHERE lower(email) = lower($1)`, email); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, email, password); !errors.Is(err, ErrNoAccount) {
		t.Errorf("a disabled account signed in: %v", err)
	}
}

// An account row can exist with no password hash -- created directly, or left
// behind by a sign-in method this product no longer has. It must not be
// reachable by guessing one.
func TestAccountWithoutAPasswordCannotAuthenticate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := unique("federated") + "@northwind.id"
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO users (email, display_name, role) VALUES ($1,$2,'operator')`,
		email, "Federated"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, email, ""); !errors.Is(err, ErrNoAccount) {
		t.Errorf("an account with no password authenticated: %v", err)
	}
}

func TestMFAEnrolmentRequiresAValidCode(t *testing.T) {
	s := testStore(t)
	email, _ := newAccount(t, s, "admin")
	ctx := context.Background()

	secret, err := s.BeginMFAEnrolment(ctx, email)
	if err != nil {
		t.Fatal(err)
	}

	// Begun is not enrolled: walking away must not leave an account whose
	// second factor nobody can produce.
	a, _ := s.Account(ctx, email)
	if a.MFAEnrolled {
		t.Error("beginning an enrolment must not mark the account enrolled")
	}

	if _, err := s.ConfirmMFAEnrolment(ctx, email, "000000"); err == nil {
		t.Error("a wrong code confirmed the enrolment")
	}

	code, _ := auth.TOTPCode(secret, time.Now())
	recovery, err := s.ConfirmMFAEnrolment(ctx, email, code)
	if err != nil {
		t.Fatalf("the correct code was refused: %v", err)
	}
	if len(recovery) != recoveryCodeCount {
		t.Errorf("got %d recovery codes, want %d", len(recovery), recoveryCodeCount)
	}
	a, _ = s.Account(ctx, email)
	if !a.MFAEnrolled {
		t.Error("a confirmed enrolment must mark the account enrolled")
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	s := testStore(t)
	email, _ := newAccount(t, s, "admin")
	ctx := context.Background()

	secret, _ := s.BeginMFAEnrolment(ctx, email)
	code, _ := auth.TOTPCode(secret, time.Now())
	recovery, err := s.ConfirmMFAEnrolment(ctx, email, code)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Account(ctx, email)

	ok, err := s.ConsumeRecoveryCode(ctx, a.ID, recovery[0])
	if err != nil || !ok {
		t.Fatalf("a fresh recovery code was refused: %v %v", ok, err)
	}
	// Spending it again must fail, or a code read over someone's shoulder is
	// a permanent second factor.
	ok, _ = s.ConsumeRecoveryCode(ctx, a.ID, recovery[0])
	if ok {
		t.Error("a recovery code was accepted twice")
	}
	// Case and surrounding space are how people actually type these.
	ok, _ = s.ConsumeRecoveryCode(ctx, a.ID, "  "+strings.ToUpper(recovery[1])+" ")
	if !ok {
		t.Error("a correctly typed code was refused for its case or spacing")
	}
	left, _ := s.RemainingRecoveryCodes(ctx, a.ID)
	if left != recoveryCodeCount-2 {
		t.Errorf("%d codes remain, want %d", left, recoveryCodeCount-2)
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, a.ID, "zzzz-zzzz-zzzz-zzzz"); ok {
		t.Error("a code that was never issued was accepted")
	}
}

// Re-enrolling must invalidate codes tied to the device being replaced.
func TestReEnrolmentReplacesRecoveryCodes(t *testing.T) {
	s := testStore(t)
	email, _ := newAccount(t, s, "admin")
	ctx := context.Background()

	secret, _ := s.BeginMFAEnrolment(ctx, email)
	code, _ := auth.TOTPCode(secret, time.Now())
	first, _ := s.ConfirmMFAEnrolment(ctx, email, code)
	a, _ := s.Account(ctx, email)

	secret2, _ := s.BeginMFAEnrolment(ctx, email)
	code2, _ := auth.TOTPCode(secret2, time.Now())
	second, err := s.ConfirmMFAEnrolment(ctx, email, code2)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, a.ID, first[0]); ok {
		t.Error("a code from the previous enrolment still works")
	}
	if ok, _ := s.ConsumeRecoveryCode(ctx, a.ID, second[0]); !ok {
		t.Error("a code from the current enrolment was refused")
	}
}

func TestSetPasswordReplacesTheOldOne(t *testing.T) {
	s := testStore(t)
	email, old := newAccount(t, s, "operator")
	ctx := context.Background()

	if err := s.SetPassword(ctx, email, "an entirely different passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, email, old); !errors.Is(err, ErrNoAccount) {
		t.Error("the old password still works after a change")
	}
	if _, err := s.Authenticate(ctx, email, "an entirely different passphrase"); err != nil {
		t.Errorf("the new password was refused: %v", err)
	}
	if err := s.SetPassword(ctx, "nobody@northwind.id", "another passphrase entirely"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("setting a password on a missing account: %v", err)
	}
}
