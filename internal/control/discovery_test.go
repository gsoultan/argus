package control

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// cleanHost removes a test host so reruns start from the same place.
func cleanHost(t *testing.T, s *Store, hostname string) {
	t.Helper()
	ctx := context.Background()
	_, _ = s.pool.Exec(ctx, `DELETE FROM agents WHERE hostname = $1`, hostname)
	_, _ = s.pool.Exec(ctx, `DELETE FROM assets WHERE agent_hostname = $1 OR hostname = $1`, hostname)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM agents WHERE hostname = $1`, hostname)
		_, _ = s.pool.Exec(ctx, `DELETE FROM assets WHERE agent_hostname = $1 OR hostname = $1`, hostname)
	})
}

func seedDiscovered(t *testing.T, s *Store, hostname string) {
	t.Helper()
	ctx := context.Background()
	// Heartbeat first, as a real agent does: it creates the row and, finding no
	// matching asset, marks it unmatched.
	if err := s.Heartbeat(ctx, hostname, "1.0.0", 0, nil); err != nil &&
		!errors.Is(err, ErrAgentUnmatched) {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := s.RecordFacts(ctx, hostname, HostFacts{
		FQDN:      hostname + ".payments.northwind.id",
		MachineID: "mid-" + hostname,
		OS:        "Debian GNU/Linux 13 (trixie)",
		Addresses: []string{"10.20.0.7"},
		SSHPorts:  []int{22, 2222},
		Accounts:  []string{"root", "ops"},
	}); err != nil {
		t.Fatalf("RecordFacts: %v", err)
	}
}

// The gap this feature exists to close: an agent running somewhere the
// inventory has no entry for is a privileged host Argus is not managing, and it
// must be visible rather than a log line.
func TestDiscoveredHostSurfacesUnmanagedMachine(t *testing.T) {
	s := testStore(t)
	cleanHost(t, s, "disc-01")
	seedDiscovered(t, s, "disc-01")

	hosts, err := s.DiscoveredHosts(context.Background(), "unreviewed")
	if err != nil {
		t.Fatalf("DiscoveredHosts: %v", err)
	}
	i := slices.IndexFunc(hosts, func(h DiscoveredHost) bool { return h.Hostname == "disc-01" })
	if i < 0 {
		t.Fatal("an agent with no matching asset did not appear in discovery")
	}
	h := hosts[i]
	if h.OS != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("OS = %q", h.OS)
	}
	if !slices.Equal(h.SSHPorts, []int{22, 2222}) {
		t.Errorf("SSHPorts = %v; a port the inventory does not know is an unmonitored way in", h.SSHPorts)
	}
	if !slices.Equal(h.Accounts, []string{"root", "ops"}) {
		t.Errorf("Accounts = %v", h.Accounts)
	}
	if h.State != "unreviewed" {
		t.Errorf("State = %q, want unreviewed", h.State)
	}
}

// Enrolment must not grant anything. Discovery reports which accounts exist; it
// does not get to decide who may assume them, or anyone able to start an agent
// could grant themselves a login.
func TestEnrolCreatesAssetWithNoPrincipals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-02")
	seedDiscovered(t, s, "disc-02")

	id, err := s.EnrolHost(ctx, "disc-02", "admin@northwind.id")
	if err != nil {
		t.Fatalf("EnrolHost: %v", err)
	}
	if id == "" {
		t.Fatal("no asset id returned")
	}

	var (
		principals []string
		agentHost  string
		discovered *string
		port       int
	)
	err = s.pool.QueryRow(ctx, `
		SELECT principals, COALESCE(agent_hostname,''), discovered_by, port
		FROM assets WHERE id = $1`, id).
		Scan(&principals, &agentHost, &discovered, &port)
	if err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if len(principals) != 0 {
		t.Errorf("enrolment granted principals %v; it must grant nothing", principals)
	}
	if agentHost != "disc-02" {
		t.Errorf("agent_hostname = %q; coverage would not be tracked", agentHost)
	}
	if discovered == nil || *discovered != "admin@northwind.id" {
		t.Error("discovered_by does not record who enrolled the host")
	}
	if port != 22 {
		t.Errorf("port = %d, want the first reported SSH port", port)
	}

	// Now matched, so it leaves the unreviewed list.
	hosts, _ := s.DiscoveredHosts(ctx, "unreviewed")
	if slices.ContainsFunc(hosts, func(h DiscoveredHost) bool { return h.Hostname == "disc-02" }) {
		t.Error("an enrolled host is still listed as unreviewed")
	}
}

func TestEnrolIsNotRepeatable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-03")
	seedDiscovered(t, s, "disc-03")

	if _, err := s.EnrolHost(ctx, "disc-03", "admin@northwind.id"); err != nil {
		t.Fatalf("first enrol: %v", err)
	}
	_, err := s.EnrolHost(ctx, "disc-03", "someone.else@northwind.id")
	if !errors.Is(err, ErrAlreadyReviewed) {
		t.Errorf("second enrol err = %v, want ErrAlreadyReviewed", err)
	}
}

// A dismissal is kept, not deleted: one that leaves no trace is
// indistinguishable from a host nobody ever looked at.
func TestIgnoreKeepsTheDecisionVisible(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-04")
	seedDiscovered(t, s, "disc-04")

	if err := s.IgnoreHost(ctx, "disc-04", "admin@northwind.id", "ephemeral CI runner"); err != nil {
		t.Fatalf("IgnoreHost: %v", err)
	}
	if err := s.IgnoreHost(ctx, "disc-04", "admin@northwind.id", "again"); !errors.Is(err, ErrAlreadyReviewed) {
		t.Errorf("second ignore err = %v, want ErrAlreadyReviewed", err)
	}

	hosts, _ := s.DiscoveredHosts(ctx, "ignored")
	i := slices.IndexFunc(hosts, func(h DiscoveredHost) bool { return h.Hostname == "disc-04" })
	if i < 0 {
		t.Fatal("a dismissed host vanished; the decision must stay visible")
	}
	if hosts[i].ReviewNote != "ephemeral CI runner" {
		t.Errorf("ReviewNote = %q; the next reviewer has to redo the work", hosts[i].ReviewNote)
	}
	if hosts[i].ReviewedBy != "admin@northwind.id" {
		t.Errorf("ReviewedBy = %q", hosts[i].ReviewedBy)
	}

	// And it is out of the queue that demands attention.
	unreviewed, _ := s.DiscoveredHosts(ctx, "unreviewed")
	if slices.ContainsFunc(unreviewed, func(h DiscoveredHost) bool { return h.Hostname == "disc-04" }) {
		t.Error("a dismissed host is still unreviewed")
	}
}

// Both directions are gaps and neither implies the other.
func TestCoverageCountsBothKindsOfGap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-05")

	before, err := s.Coverage(ctx)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	seedDiscovered(t, s, "disc-05")

	after, err := s.Coverage(ctx)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if after.UnreviewedHosts != before.UnreviewedHosts+1 {
		t.Errorf("UnreviewedHosts %d → %d, want +1", before.UnreviewedHosts, after.UnreviewedHosts)
	}
	if after.Assets != before.Assets {
		t.Errorf("an unmanaged host changed the asset count: %d → %d", before.Assets, after.Assets)
	}
}

func TestRecordFactsRequiresHostname(t *testing.T) {
	s := testStore(t)
	if err := s.RecordFacts(context.Background(), "", HostFacts{}); err == nil {
		t.Error("RecordFacts accepted an empty hostname")
	}
}
