package control

import (
	"context"
	"testing"
)

/*
Email address case.

users.email carries a UNIQUE constraint, which is case-sensitive, while every
lookup in accounts.go matches on lower(email). Those two rules disagree, and
the gap between them is an account nobody can see and a password that belongs
to more than one row.
*/

// Two addresses differing only in case must be one account, not two.
func TestAccountEmailsAreCaseInsensitive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lower := unique("alice") + "@corp.example"
	upper := "A" + lower[1:]

	id1, err := s.CreateAccount(ctx, lower, "Alice Admin", "admin", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("create %s: %v", lower, err)
	}
	id2, err := s.CreateAccount(ctx, upper, "Alice Viewer", "viewer", "a-completely-different-pw")
	if err != nil {
		t.Fatalf("create %s: %v", upper, err)
	}
	if id1 != id2 {
		t.Errorf("%s and %s created two accounts (%s, %s); sign-in matches on "+
			"lower(email), so both answer to the same address", lower, upper, id1, id2)
	}
}

// Whichever account a lookup finds, it must be the same one every time and its
// role must be the role that was set for it.
func TestSignInIsNotAmbiguousAcrossCase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lower := unique("bob") + "@corp.example"
	upper := "B" + lower[1:]

	if _, err := s.CreateAccount(ctx, lower, "Bob Admin", "admin", "first-password-here"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateAccount(ctx, upper, "Bob Viewer", "viewer", "second-password-here"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Both passwords must not open the same address. If they do, an account
	// created at a different case has silently become another way in.
	first, err1 := s.Authenticate(ctx, lower, "first-password-here")
	second, err2 := s.Authenticate(ctx, lower, "second-password-here")

	if err1 == nil && err2 == nil {
		t.Errorf("one address accepted two different passwords (roles %q and %q)",
			first.Role, second.Role)
	}
	// And the surviving identity must be stable, not whichever row Postgres
	// happened to return.
	for i := 0; i < 5; i++ {
		a, err := s.Authenticate(ctx, lower, "first-password-here")
		b, err2 := s.Authenticate(ctx, lower, "first-password-here")
		if err != nil || err2 != nil {
			continue
		}
		if a.ID != b.ID || a.Role != b.Role {
			t.Fatalf("the same credential resolved to two identities: %s/%s and %s/%s",
				a.ID, a.Role, b.ID, b.Role)
		}
	}
}
