package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These run against a real Postgres because the behaviours under test are
// database behaviours: the atomicity of ticket redemption, the row lock that
// prevents two approvers deciding at once, the advisory lock serialising the
// audit chain. A fake store would assert that the fake works.
//
//	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable go test ./internal/control/
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set; skipping database tests")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// unique keeps parallel runs and repeated runs from colliding, since these
// share one database rather than getting a fresh one each time.
func unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

/* ── Ticket redemption ───────────────────────────────────────────────────── */

func TestRedeemTicketIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := unique("ticket")
	exp := time.Now().Add(time.Minute)

	used, err := s.RedeemTicket(ctx, id, "u@x", "host", "ops", exp)
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if used {
		t.Error("a fresh ticket reported as already used")
	}

	used, err = s.RedeemTicket(ctx, id, "u@x", "host", "ops", exp)
	if err != nil {
		t.Fatalf("second redeem: %v", err)
	}
	if !used {
		t.Error("a replayed ticket was not detected")
	}
}

// The insert is the check, so concurrent redemptions must yield exactly one
// winner. A read-then-write would let several through.
func TestConcurrentRedemptionAdmitsExactlyOne(t *testing.T) {
	s := testStore(t)
	id := unique("concurrent")
	exp := time.Now().Add(time.Minute)

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			used, err := s.RedeemTicket(context.Background(), id, "u", "h", "ops", exp)
			if err == nil && !used {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if fresh != 1 {
		t.Errorf("%d concurrent redemptions saw the ticket as fresh, want exactly 1", fresh)
	}
}

func TestSweepDropsOnlyExpiredTickets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	live := unique("live")
	stale := unique("stale")
	_, _ = s.RedeemTicket(ctx, live, "u", "h", "ops", time.Now().Add(time.Hour))
	_, _ = s.RedeemTicket(ctx, stale, "u", "h", "ops", time.Now().Add(-2*time.Hour))

	if _, err := s.SweepTickets(ctx); err != nil {
		t.Fatalf("SweepTickets: %v", err)
	}

	// The live one must still be recognised as used; forgetting it would
	// re-open the replay window.
	used, _ := s.RedeemTicket(ctx, live, "u", "h", "ops", time.Now().Add(time.Hour))
	if !used {
		t.Error("sweep discarded a ticket that has not expired")
	}
}

/* ── Host key pins ───────────────────────────────────────────────────────── */

func TestPinHostKeyRefusesToOverwrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := unique("host")

	if _, err := s.PinHostKey(ctx, HostKeyPin{
		Host: host, Fingerprint: "SHA256:original", PinnedBy: "tofu",
	}); err != nil {
		t.Fatalf("first pin: %v", err)
	}

	// Overwriting silently is how a host-key mismatch turns into no alert.
	conflict, err := s.PinHostKey(ctx, HostKeyPin{
		Host: host, Fingerprint: "SHA256:different", PinnedBy: "tofu",
	})
	if err != nil {
		t.Fatalf("second pin: %v", err)
	}
	if conflict == nil {
		t.Fatal("a different fingerprint overwrote the pin without conflict")
	}
	if conflict.Fingerprint != "SHA256:original" {
		t.Errorf("conflict reports %q, want the original", conflict.Fingerprint)
	}
}

// Two gateways pinning the same key on first contact is a benign race, not a
// mismatch — refusing would make trust-on-first-use unusable with more than one
// gateway.
func TestPinningTheSameKeyTwiceIsBenign(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := unique("host")
	pin := HostKeyPin{Host: host, Fingerprint: "SHA256:same", PinnedBy: "tofu"}

	if _, err := s.PinHostKey(ctx, pin); err != nil {
		t.Fatal(err)
	}
	conflict, err := s.PinHostKey(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Error("re-pinning an identical key was reported as a conflict")
	}
}

func TestRepinReplacesDeliberately(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := unique("host")

	_, _ = s.PinHostKey(ctx, HostKeyPin{Host: host, Fingerprint: "SHA256:old"})
	if err := s.RepinHostKey(ctx, HostKeyPin{
		Host: host, Fingerprint: "SHA256:new", PinnedBy: "dewi.p@northwind.id",
	}); err != nil {
		t.Fatalf("RepinHostKey: %v", err)
	}

	got, err := s.HostKeyPin(ctx, host)
	if err != nil || got == nil {
		t.Fatalf("HostKeyPin: %v", err)
	}
	if got.Fingerprint != "SHA256:new" || got.PinnedBy != "dewi.p@northwind.id" {
		t.Errorf("pin = %+v", got)
	}
}

func TestHostKeyPinAbsentIsNotAnError(t *testing.T) {
	s := testStore(t)
	pin, err := s.HostKeyPin(context.Background(), unique("never-pinned"))
	if err != nil {
		t.Fatalf("err = %v, want nil for an unpinned host", err)
	}
	if pin != nil {
		t.Errorf("pin = %+v, want nil", pin)
	}
}

/* ── Access requests ─────────────────────────────────────────────────────── */

func newRequest(t *testing.T, s *Store, requester string) AccessRequest {
	t.Helper()
	r, err := s.CreateRequest(context.Background(), AccessRequest{
		RequesterEmail:  requester,
		AssetHostnames:  []string{unique("host")},
		Principal:       "root",
		Justification:   "INC-1234 investigating a wedged settlement worker on the primary.",
		DurationMinutes: 60,
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	return r
}

// Without this the approval step is decoration: anyone could grant themselves
// root and the audit log would show a tidy approval by the requester.
func TestSelfApprovalIsRefused(t *testing.T) {
	s := testStore(t)
	r := newRequest(t, s, "operator@northwind.id")

	_, err := s.DecideRequest(context.Background(), r.ID, "approved", "", "operator@northwind.id")
	if !errors.Is(err, ErrSelfApproval) {
		t.Errorf("err = %v, want ErrSelfApproval", err)
	}
}

func TestApprovalByAnotherPersonSucceeds(t *testing.T) {
	s := testStore(t)
	r := newRequest(t, s, "operator@northwind.id")

	out, err := s.DecideRequest(context.Background(), r.ID, "approved", "ok", "approver@northwind.id")
	if err != nil {
		t.Fatalf("DecideRequest: %v", err)
	}
	if out.State != "approved" || out.ExpiresAt == nil {
		t.Errorf("approved request has no expiry: %+v", out)
	}
	if out.ExpiresAt.Before(time.Now()) {
		t.Error("the grant expired before it was issued")
	}
}

// Two approvers clicking at once must not both decide, or the audit log records
// a decision that never took effect.
func TestConcurrentDecisionsAdmitOne(t *testing.T) {
	s := testStore(t)
	r := newRequest(t, s, "operator@northwind.id")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	decided := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := s.DecideRequest(context.Background(), r.ID, "approved", "ok",
				fmt.Sprintf("approver%d@northwind.id", i))
			if err == nil {
				mu.Lock()
				decided++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if decided != 1 {
		t.Errorf("%d concurrent decisions succeeded, want exactly 1", decided)
	}
}

func TestDecidingTwiceIsRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := newRequest(t, s, "operator@northwind.id")

	if _, err := s.DecideRequest(ctx, r.ID, "approved", "ok", "a@x.id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideRequest(ctx, r.ID, "denied", "changed my mind", "b@x.id"); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("err = %v, want ErrAlreadyDecided", err)
	}
}

func TestRequestValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := AccessRequest{
		RequesterEmail: "u@x.id", AssetHostnames: []string{"h"},
		Principal: "root", Justification: "a sufficiently long justification here",
		DurationMinutes: 60,
	}

	t.Run("no hosts", func(t *testing.T) {
		r := base
		r.AssetHostnames = nil
		if _, err := s.CreateRequest(ctx, r); err == nil {
			t.Error("accepted a request with no hosts")
		}
	})

	// An approver cannot act on "need access".
	t.Run("thin justification", func(t *testing.T) {
		r := base
		r.Justification = "need it"
		if _, err := s.CreateRequest(ctx, r); err == nil {
			t.Error("accepted a justification too short to decide on")
		}
	})

	t.Run("duration beyond the cap", func(t *testing.T) {
		r := base
		r.DurationMinutes = MaxDurationMinutes + 1
		if _, err := s.CreateRequest(ctx, r); err == nil {
			t.Error("accepted a duration above the policy cap")
		}
	})
}

/* ── Grants ──────────────────────────────────────────────────────────────── */

func TestActiveGrantMatchesAndExpires(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := unique("host")
	requester := "operator@northwind.id"

	r, err := s.CreateRequest(ctx, AccessRequest{
		RequesterEmail: requester, AssetHostnames: []string{host},
		Principal: "root", Justification: "INC-1234 a long enough justification.",
		DurationMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideRequest(ctx, r.ID, "approved", "ok", "approver@x.id"); err != nil {
		t.Fatal(err)
	}

	granted, expires, err := s.ActiveGrant(ctx, requester, host, "root")
	if err != nil {
		t.Fatal(err)
	}
	if !granted || expires == nil {
		t.Fatal("an approved grant was not found")
	}

	// Scope: the grant must not extend to another person, host or principal.
	for _, c := range []struct{ who, host, principal, desc string }{
		{"someone.else@x.id", host, "root", "another person"},
		{requester, unique("other-host"), "root", "another host"},
		{requester, host, "ops", "another principal"},
	} {
		g, _, err := s.ActiveGrant(ctx, c.who, c.host, c.principal)
		if err != nil {
			t.Fatal(err)
		}
		if g {
			t.Errorf("the grant leaked to %s", c.desc)
		}
	}
}

func TestDeniedRequestGrantsNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := unique("host")
	requester := "operator@northwind.id"

	r, _ := s.CreateRequest(ctx, AccessRequest{
		RequesterEmail: requester, AssetHostnames: []string{host},
		Principal: "root", Justification: "INC-1234 a long enough justification.",
		DurationMinutes: 60,
	})
	if _, err := s.DecideRequest(ctx, r.ID, "denied", "scope too broad", "approver@x.id"); err != nil {
		t.Fatal(err)
	}

	granted, _, _ := s.ActiveGrant(ctx, requester, host, "root")
	if granted {
		t.Error("a denied request produced an active grant")
	}
}

/* ── Audit chain ─────────────────────────────────────────────────────────── */

func TestAuditChainLinksAndVerifies(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var prev string
	for i := 0; i < 5; i++ {
		e, err := s.AppendAudit(ctx, AuditEvent{
			Action: "test.event", Severity: "info",
			ActorEmail: "u@x.id", Target: unique("t"),
			Detail: fmt.Sprintf("event %d", i),
		})
		if err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
		if e.Hash == "" || e.PrevHash == "" {
			t.Fatal("event has no chain hashes")
		}
		if prev != "" && e.PrevHash != prev {
			t.Errorf("event %d does not link to its predecessor", i)
		}
		prev = e.Hash
	}
}

// Concurrent appends must not produce two events claiming the same
// predecessor, which would break verification for everything after them.
func TestConcurrentAuditAppendsStayChained(t *testing.T) {
	s := testStore(t)

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = s.AppendAudit(context.Background(), AuditEvent{
				Action: "concurrent.test", ActorEmail: "u@x.id",
				Detail: fmt.Sprintf("%d", i),
			})
		}(i)
	}
	wg.Wait()

	events, err := s.AuditEvents(context.Background(), 500)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}

	// Walk oldest to newest and confirm every link points at its predecessor.
	seenPrev := map[string]bool{}
	for i := len(events) - 1; i > 0; i-- {
		older, newer := events[i], events[i-1]
		if newer.PrevHash != older.Hash {
			t.Fatalf("chain broken between seq %d and %d", older.Seq, newer.Seq)
		}
		if seenPrev[newer.PrevHash] {
			t.Fatalf("two events claim seq %d as their predecessor", older.Seq)
		}
		seenPrev[newer.PrevHash] = true
	}
}

/* ── Audit chain verification ────────────────────────────────────────────── */

func TestVerifyAuditChainAcceptsAnIntactLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.AppendAudit(ctx, AuditEvent{
			Action: "verify.test", ActorEmail: "u@x.id", Detail: unique("d"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	v, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !v.OK {
		t.Errorf("an intact chain failed verification at seq %d: %s", v.BrokenAt, v.Detail)
	}
	if v.Checked == 0 {
		t.Error("verified zero events")
	}
}

// Editing a record in place is the tampering this exists to catch.
func TestVerifyAuditChainDetectsAnEditedRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	e, err := s.AppendAudit(ctx, AuditEvent{
		Action: "verify.tamper", ActorEmail: "u@x.id", Detail: "original detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Append after it, so the break is mid-chain rather than at the tail.
	if _, err := s.AppendAudit(ctx, AuditEvent{Action: "verify.after", ActorEmail: "u@x.id"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE audit_events SET detail = 'quietly rewritten' WHERE seq = $1`, e.Seq); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`UPDATE audit_events SET detail = $2 WHERE seq = $1`, e.Seq, "original detail")
	})

	v, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if v.OK {
		t.Fatal("an edited record passed verification")
	}
	if v.BrokenAt != e.Seq {
		t.Errorf("BrokenAt = %d, want %d", v.BrokenAt, e.Seq)
	}
	// The message has to tell an operator what happened, not just that
	// something did.
	if !strings.Contains(v.Detail, "modified after it was written") {
		t.Errorf("detail does not explain the failure: %q", v.Detail)
	}
}

// A removed row breaks the links even though every remaining row is internally
// consistent — a distinct failure from an edit, and worth reporting as such.
func TestVerifyAuditChainDetectsARemovedRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var seqs []int64
	for i := 0; i < 3; i++ {
		e, err := s.AppendAudit(ctx, AuditEvent{
			Action: "verify.removal", ActorEmail: "u@x.id", Detail: unique("d"),
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, e.Seq)
	}

	// Delete the middle one.
	var saved AuditEvent
	if err := s.pool.QueryRow(ctx, `
		SELECT seq, at, action, severity, actor_email, target, detail, prev_hash, hash
		FROM audit_events WHERE seq = $1`, seqs[1]).
		Scan(&saved.Seq, &saved.At, &saved.Action, &saved.Severity, &saved.ActorEmail,
			&saved.Target, &saved.Detail, &saved.PrevHash, &saved.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM audit_events WHERE seq = $1`, seqs[1]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `
			INSERT INTO audit_events (seq, at, action, severity, actor_email, target, detail, prev_hash, hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
			saved.Seq, saved.At, saved.Action, saved.Severity, saved.ActorEmail,
			saved.Target, saved.Detail, saved.PrevHash, saved.Hash)
	})

	v, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if v.OK {
		t.Fatal("a removed record passed verification")
	}
	if !strings.Contains(v.Detail, "removed, inserted or reordered") {
		t.Errorf("detail does not identify a structural break: %q", v.Detail)
	}
}

// The audit chain must verify for a timestamp with nanosecond precision.
//
// chainHash formats with RFC3339Nano but TIMESTAMPTZ stores microseconds, so a
// hash computed over the in-memory value could never be recomputed from the
// stored row. Go's clock is microsecond-granular on macOS and nanosecond-
// granular on Linux, so this was invisible on a developer's machine and broke
// verification for nearly every event in production. The timestamp here is
// explicit rather than time.Now(), so the test fails on any platform.
func TestAuditChainVerifiesWithNanosecondTimestamps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := range 3 {
		at := time.Date(2026, 9, 6, 12, 0, i, 123456789, time.UTC)
		e, err := s.AppendAudit(ctx, AuditEvent{
			At: at, Action: "verify.nanos", ActorEmail: "u@x.id", Detail: unique("d"),
		})
		if err != nil {
			t.Fatal(err)
		}
		// What was hashed must be what can be read back.
		if e.At.Nanosecond()%1000 != 0 {
			t.Errorf("stored timestamp keeps sub-microsecond digits: %d ns", e.At.Nanosecond())
		}
	}

	v, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !v.OK {
		t.Fatalf("a chain written with nanosecond timestamps failed to verify at seq %d: %s",
			v.BrokenAt, v.Detail)
	}
}
