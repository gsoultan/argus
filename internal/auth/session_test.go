package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "a-test-signing-secret-of-sufficient-length"

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestShortSecretIsRejected(t *testing.T) {
	// A short secret makes offline brute force practical against a signature an
	// attacker can capture from any request.
	if _, err := NewSigner("too-short"); err == nil {
		t.Error("accepted a signing secret shorter than 32 characters")
	}
}

/* ── Sessions ────────────────────────────────────────────────────────────── */

func TestSessionRoundTrip(t *testing.T) {
	s := newSigner(t)
	token, err := s.IssueSession(Session{
		Email: "dewi.p@northwind.id", Name: "Dewi P.", Role: "admin", Subject: "okta|1",
	}, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	got, err := s.VerifySession(token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if got.Email != "dewi.p@northwind.id" || got.Role != "admin" {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// The one that matters: a session must not be forgeable by editing the payload.
func TestTamperedSessionIsRejected(t *testing.T) {
	s := newSigner(t)
	token, _ := s.IssueSession(Session{Email: "operator@northwind.id", Role: "operator"}, time.Hour)

	body, sig, _ := strings.Cut(token, ".")

	t.Run("payload swapped for an admin session", func(t *testing.T) {
		// Craft a payload claiming admin and attach the original signature.
		forged, _ := s.sign(Session{
			Email: "operator@northwind.id", Role: "owner",
			ExpiresAt: time.Now().Add(time.Hour),
		})
		forgedBody, _, _ := strings.Cut(forged, ".")
		if _, err := s.VerifySession(forgedBody + "." + sig); err == nil {
			t.Error("a payload with someone else's signature was accepted")
		}
	})

	t.Run("signature altered", func(t *testing.T) {
		bad := sig[:len(sig)-1] + "A"
		if bad == sig {
			bad = sig[:len(sig)-1] + "B"
		}
		if _, err := s.VerifySession(body + "." + bad); err == nil {
			t.Error("an altered signature was accepted")
		}
	})

	t.Run("signed by a different secret", func(t *testing.T) {
		other, _ := NewSigner("a-completely-different-secret-of-length")
		otherToken, _ := other.IssueSession(Session{Email: "attacker", Role: "owner"}, time.Hour)
		if _, err := s.VerifySession(otherToken); err == nil {
			t.Error("a session signed by another key was accepted")
		}
	})

	t.Run("structurally malformed", func(t *testing.T) {
		for _, bad := range []string{"", ".", "nodot", "a.b.c", "!!!.???"} {
			if _, err := s.VerifySession(bad); err == nil {
				t.Errorf("accepted malformed token %q", bad)
			}
		}
	})
}

func TestExpiredSessionIsRejected(t *testing.T) {
	s := newSigner(t)
	token, err := s.IssueSession(Session{Email: "x@y.z"}, -time.Minute)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if _, err := s.VerifySession(token); !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

/* ── Tickets ─────────────────────────────────────────────────────────────── */

func TestTicketIsSingleUse(t *testing.T) {
	s := newSigner(t)
	token, _ := s.IssueTicket("dewi.p@northwind.id", "pay-01", "ops", time.Minute)
	ctx := context.Background()

	if _, err := s.RedeemTicket(ctx, token, "pay-01", "ops"); err != nil {
		t.Fatalf("first redemption failed: %v", err)
	}
	if _, err := s.RedeemTicket(ctx, token, "pay-01", "ops"); !errors.Is(err, ErrUsed) {
		t.Errorf("second redemption err = %v, want ErrUsed", err)
	}
}

// A ticket for a staging box must not open a production one.
func TestTicketIsBoundToTargetAndPrincipal(t *testing.T) {
	ctx := context.Background()

	t.Run("wrong target", func(t *testing.T) {
		s := newSigner(t)
		token, _ := s.IssueTicket("u", "pay-01", "ops", time.Minute)
		if _, err := s.RedeemTicket(ctx, token, "prod-db-01", "ops"); err == nil {
			t.Error("a ticket for pay-01 opened prod-db-01")
		}
	})

	t.Run("wrong principal", func(t *testing.T) {
		s := newSigner(t)
		token, _ := s.IssueTicket("u", "pay-01", "ops", time.Minute)
		if _, err := s.RedeemTicket(ctx, token, "pay-01", "root"); err == nil {
			t.Error("a ticket for ops opened a root session")
		}
	})

	// A failed scope check must not burn the ticket, or anyone who can guess a
	// target name can invalidate someone else's session.
	t.Run("a rejected scope does not consume the ticket", func(t *testing.T) {
		s := newSigner(t)
		token, _ := s.IssueTicket("u", "pay-01", "ops", time.Minute)
		_, _ = s.RedeemTicket(ctx, token, "wrong-host", "ops")
		if _, err := s.RedeemTicket(ctx, token, "pay-01", "ops"); err != nil {
			t.Errorf("legitimate redemption failed after a rejected one: %v", err)
		}
	})
}

func TestExpiredTicketIsRejected(t *testing.T) {
	s := newSigner(t)
	token, _ := s.IssueTicket("u", "pay-01", "ops", -time.Second)
	if _, err := s.RedeemTicket(context.Background(), token, "pay-01", "ops"); !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestTicketIdsAreUnique(t *testing.T) {
	s := newSigner(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		token, _ := s.IssueTicket("u", "h", "ops", time.Minute)
		var tk Ticket
		if err := s.verify(token, &tk); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if seen[tk.ID] {
			// A repeated id would make one ticket burn another.
			t.Fatalf("duplicate ticket id %q after %d issues", tk.ID, i)
		}
		seen[tk.ID] = true
	}
}

/* ── Shared redemption ───────────────────────────────────────────────────── */

type fakeRedeemer struct {
	mu       sync.Mutex
	seen     map[string]bool
	failWith error
}

func (f *fakeRedeemer) Redeem(_ context.Context, t Ticket) (bool, error) {
	if f.failWith != nil {
		return false, f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[t.ID] {
		return true, nil
	}
	f.seen[t.ID] = true
	return false, nil
}

// Two gateways sharing a redeemer must not both accept the same ticket.
func TestSharedRedemptionSpansInstances(t *testing.T) {
	shared := &fakeRedeemer{}

	gatewayA := newSigner(t)
	gatewayB := newSigner(t)
	gatewayA.SetRedeemer(shared)
	gatewayB.SetRedeemer(shared)

	token, _ := gatewayA.IssueTicket("u", "pay-01", "ops", time.Minute)
	ctx := context.Background()

	if _, err := gatewayA.RedeemTicket(ctx, token, "pay-01", "ops"); err != nil {
		t.Fatalf("gateway A: %v", err)
	}
	if _, err := gatewayB.RedeemTicket(ctx, token, "pay-01", "ops"); !errors.Is(err, ErrUsed) {
		t.Errorf("gateway B accepted a ticket burned on A: err = %v", err)
	}
}

// If the shared store cannot answer, refusing is the only safe response:
// assuming "fresh" would make every gateway accept replays during an outage.
func TestUnreachableRedeemerRefuses(t *testing.T) {
	s := newSigner(t)
	s.SetRedeemer(&fakeRedeemer{failWith: errors.New("connection refused")})

	token, _ := s.IssueTicket("u", "pay-01", "ops", time.Minute)
	_, err := s.RedeemTicket(context.Background(), token, "pay-01", "ops")
	if err == nil {
		t.Fatal("a ticket was accepted while the shared store was unreachable")
	}
	if !strings.Contains(err.Error(), "cannot verify") {
		t.Errorf("error should say the check could not be made, got: %v", err)
	}
}

func TestSharedRedemptionIsReported(t *testing.T) {
	s := newSigner(t)
	if s.SharedRedemption() {
		t.Error("reports shared redemption with no redeemer set")
	}
	s.SetRedeemer(&fakeRedeemer{})
	if !s.SharedRedemption() {
		t.Error("does not report shared redemption after one is set")
	}
}

// Concurrent redemptions of one ticket must yield exactly one success.
func TestConcurrentRedemptionAdmitsOne(t *testing.T) {
	s := newSigner(t)
	token, _ := s.IssueTicket("u", "pay-01", "ops", time.Minute)

	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.RedeemTicket(context.Background(), token, "pay-01", "ops"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("%d concurrent redemptions succeeded, want exactly 1", successes)
	}
}
