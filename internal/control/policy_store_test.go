package control

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// These run against a real Postgres for the same reason the rest of store_test
// does: the behaviours are database behaviours. The single-row constraint, the
// seeded defaults and the whole-document replace are all things a fake store
// would simply agree with.
//
//	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable go test ./internal/control/

func TestGatewayPolicySeedsTheClosedDefaults(t *testing.T) {
	s := testStore(t)
	p, err := s.GatewayPolicy(context.Background())
	if err != nil {
		t.Fatalf("GatewayPolicy: %v", err)
	}
	// Migration 006 seeds the row, so a read on a fresh database returns the
	// safe position rather than a missing-row error the caller has to interpret.
	if p.AllowLocalForward || p.AllowRemoteForward || p.AllowAgentForward || p.AllowX11Forward {
		t.Errorf("no forwarding channel may be open on a fresh database: %+v", p)
	}
	if !p.ProxySftpSubsystem || !p.FailClosedOnRecordingLoss ||
		!p.RequireEbpfForRoot {
		t.Errorf("every recording guarantee must be on by default: %+v", p)
	}
}

func TestSaveGatewayPolicyRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original, err := s.GatewayPolicy(ctx)
	if err != nil {
		t.Fatalf("GatewayPolicy: %v", err)
	}
	// Restore, so this test does not leave the shared database loosened for
	// whatever runs next.
	t.Cleanup(func() {
		if _, err := s.SaveGatewayPolicy(context.Background(), original, "test-cleanup"); err != nil {
			t.Errorf("restore policy: %v", err)
		}
	})

	next := original
	next.AllowAgentForward = true
	next.ProxySftpSubsystem = false

	saved, err := s.SaveGatewayPolicy(ctx, next, "lin@northwind.id")
	if err != nil {
		t.Fatalf("SaveGatewayPolicy: %v", err)
	}
	if !saved.AllowAgentForward || saved.ProxySftpSubsystem {
		t.Errorf("returned policy does not reflect the write: %+v", saved)
	}
	if saved.UpdatedBy != "lin@northwind.id" {
		t.Errorf("updatedBy = %q, want the actor", saved.UpdatedBy)
	}
	if saved.UpdatedAt == "" {
		t.Error("updatedAt should be stamped on write")
	}

	// The value the caller gets back must be the value a later reader sees;
	// returning the submitted document instead would hide a field the database
	// refused to change.
	reread, err := s.GatewayPolicy(ctx)
	if err != nil {
		t.Fatalf("GatewayPolicy: %v", err)
	}
	if !reflect.DeepEqual(reread, saved) {
		t.Errorf("re-read differs from the save response:\n saved  %+v\n reread %+v", saved, reread)
	}
}

// The whole document is written, so a field left false in the submission
// genuinely becomes false rather than merging with what was already stored.
func TestSaveGatewayPolicyReplacesRatherThanMerges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original, err := s.GatewayPolicy(ctx)
	if err != nil {
		t.Fatalf("GatewayPolicy: %v", err)
	}
	t.Cleanup(func() { _, _ = s.SaveGatewayPolicy(context.Background(), original, "test-cleanup") })

	opened := original
	opened.AllowLocalForward = true
	opened.AllowX11Forward = true
	if _, err := s.SaveGatewayPolicy(ctx, opened, "first@northwind.id"); err != nil {
		t.Fatalf("SaveGatewayPolicy: %v", err)
	}

	// A second admin submits a document with only X11 open. Local forwarding
	// must close, not survive because the first write had set it.
	second := DefaultGatewayPolicy()
	second.AllowX11Forward = true
	saved, err := s.SaveGatewayPolicy(ctx, second, "second@northwind.id")
	if err != nil {
		t.Fatalf("SaveGatewayPolicy: %v", err)
	}
	if saved.AllowLocalForward {
		t.Error("local forwarding survived a write that did not include it — this is a merge, not a replace")
	}
	if !saved.AllowX11Forward {
		t.Error("X11 forwarding should be open after the second write")
	}
}

// The primary key is what keeps "the policy" from becoming a question of which
// row you read.
func TestGatewayPolicyTableHoldsExactlyOneRow(t *testing.T) {
	s := testStore(t)
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gateway_policy`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("gateway_policy holds %d rows, want exactly 1", n)
	}
	_, err := s.pool.Exec(context.Background(), `INSERT INTO gateway_policy (id) VALUES (TRUE)`)
	if err == nil {
		t.Fatal("a second row was accepted; the single-row constraint is not enforced")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Logf("insert refused with: %v", err)
	}
}

// The audit entry is the record of a policy change, so it has to land.
func TestPolicyChangeIsAuditable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before := DefaultGatewayPolicy()
	after := before
	after.AllowRemoteForward = true

	detail, loosened := describePolicyChanges(DiffGatewayPolicy(before, after))
	if !loosened {
		t.Fatal("opening remote forwarding is a loosening")
	}
	ev, err := s.AppendAudit(ctx, AuditEvent{
		Action:     "policy.change",
		Severity:   "warning",
		ActorEmail: "lin@northwind.id",
		Target:     "gateway",
		Detail:     detail,
	})
	if err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if ev.Hash == "" || ev.PrevHash == "" {
		t.Error("a policy change must be chained like every other audit event")
	}
	if !strings.Contains(ev.Detail, "remote port forwarding") {
		t.Errorf("audit detail should name the field: %q", ev.Detail)
	}
}
