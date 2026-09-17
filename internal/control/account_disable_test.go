package control

import (
	"context"
	"errors"
	"testing"
)

/*
Revoking an account.

disabled_at was added with the local accounts, and every sign-in has read it
since: a disabled account cannot authenticate, does not count toward
accountsExist, and does not count as an administrator. Nothing ever wrote it.
CountAdmins was written "so the console can warn before the last one is removed
or demoted" and had no callers. The control was designed and never built, so a
deployment could create an account and never revoke one.
*/

func TestDisablingAnAccountStopsItSigningIn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email, password := newAccount(t, s, "operator")

	// Precondition: it works before.
	if _, err := s.Authenticate(ctx, email, password); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if err := s.SetAccountDisabled(ctx, email, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.Authenticate(ctx, email, password); err == nil {
		t.Error("a disabled account still authenticated; the column is read " +
			"everywhere and was written nowhere, which is the bug this closes")
	}

	// And back, or revocation is a one-way door that needs SQL to undo.
	if err := s.SetAccountDisabled(ctx, email, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := s.Authenticate(ctx, email, password); err != nil {
		t.Errorf("re-enabling did not restore sign-in: %v", err)
	}
}

// The guard CountAdmins was written for.
func TestTheLastAdministratorCannotBeDisabled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// This database is shared and already has administrators, so the case has
	// to be built rather than assumed: disable every other one for the length
	// of the test, leaving exactly one.
	solo, _ := newAccount(t, s, "admin")
	var parked []string
	rows, err := s.pool.Query(ctx,
		`SELECT email FROM users WHERE role IN ('owner','admin')
		   AND disabled_at IS NULL AND lower(email) <> lower($1)`, solo)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		parked = append(parked, e)
	}
	rows.Close()
	for _, e := range parked {
		if err := s.SetAccountDisabled(ctx, e, true); err != nil {
			t.Fatalf("parking %s: %v", e, err)
		}
	}
	t.Cleanup(func() {
		for _, e := range parked {
			_ = s.SetAccountDisabled(context.Background(), e, false)
		}
	})

	err = s.SetAccountDisabled(ctx, solo, true)
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last administrator returned %v, want ErrLastAdmin -- "+
			"a deployment nobody can administer is recovered with direct SQL, "+
			"which is what this command exists to avoid needing", err)
	}
	// And it is genuinely still usable.
	if n, err := s.CountAdmins(ctx); err != nil || n == 0 {
		t.Errorf("CountAdmins = %d (%v) after the refusal; the account should "+
			"still be active", n, err)
	}
}

// A non-administrator is not protected by the guard.
func TestANonAdminIsNotTheLastAdministrator(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email, _ := newAccount(t, s, "auditor")
	if err := s.SetAccountDisabled(ctx, email, true); err != nil {
		t.Errorf("disabling an auditor was refused: %v", err)
	}
}

func TestDisablingReportsWhatItDidNotDo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email, _ := newAccount(t, s, "operator")

	if err := s.SetAccountDisabled(ctx, email, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountDisabled(ctx, email, true); !errors.Is(err, ErrAlreadyInState) {
		t.Errorf("disabling twice returned %v, want ErrAlreadyInState", err)
	}
	if err := s.SetAccountDisabled(ctx, "nobody-"+email, true); !errors.Is(err, ErrNoAccount) {
		t.Errorf("disabling an unknown account returned %v, want ErrNoAccount", err)
	}
}

// A listing shows revoked accounts rather than omitting them.
func TestTheListingShowsDisabledAccounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email, _ := newAccount(t, s, "operator")
	if err := s.SetAccountDisabled(ctx, email, true); err != nil {
		t.Fatal(err)
	}

	accounts, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.Email != email {
			continue
		}
		if !a.Disabled {
			t.Error("the account is listed as active after being disabled")
		}
		if a.DisabledAt == nil {
			t.Error("the listing does not say when it was revoked")
		}
		return
	}
	t.Error("a disabled account was omitted from the listing; an operator " +
		"would conclude it never existed rather than that it was revoked")
}
