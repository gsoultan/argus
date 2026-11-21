package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The fleet view reads a summary of each stored report in SQL instead of
// decoding the whole report in Go. Whatever the report held, the two have to
// give the fleet row the same numbers.
func TestThePackageSummaryMatchesTheWholeReport(t *testing.T) {
	e := packagesAPI(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	flagged := pkgReport(now,
		PackageUpdate{Name: "openssl", Available: "3.0.13-2", Security: true},
		PackageUpdate{Name: "libssl3", Available: "3.0.13-2", Security: true},
		PackageUpdate{Name: "curl", Available: "8.5.0-2"})
	flagged.RebootRequired = true
	// dnf counts security fixes without saying which package carries them.
	counted := pkgReport(now, PackageUpdate{Name: "kernel", Available: "6.1"})
	counted.Manager, counted.SecurityPatches = "dnf", 4
	failed := pkgReport(now)
	failed.Error = "apt-get update: could not resolve archive.ubuntu.com"
	reports := []*PackageReport{flagged, counted, failed, pkgReport(now)}

	ids := map[string]*PackageReport{}
	for _, r := range reports {
		srv, cred := e.server(t)
		e.beat(t, cred, srv.Hostname, &PackageBeat{Supported: true, Report: r})
		ids[srv.ID] = r
	}

	rows, err := e.s.PackageFleet(ctx)
	if err != nil {
		t.Fatalf("PackageFleet: %v", err)
	}
	seen := 0
	for _, row := range rows {
		sent, ok := ids[row.AssetID]
		if !ok {
			continue
		}
		seen++
		// What the old decode produced, from what the database stored.
		var stored []byte
		if err := e.s.pool.QueryRow(ctx, `
			SELECT g.packages FROM agents g JOIN assets a ON g.hostname = split_part(a.hostname, '.', 1)
			                                              OR g.hostname = a.hostname
			 WHERE a.id = $1 LIMIT 1`, row.AssetID).Scan(&stored); err != nil {
			t.Fatalf("stored report: %v", err)
		}
		var want PackageReport
		if err := json.Unmarshal(stored, &want); err != nil {
			t.Fatalf("decode stored report: %v", err)
		}
		if row.CheckedAt == nil || !row.CheckedAt.Equal(want.CheckedAt) {
			t.Errorf("%s: checked %v, want %v", row.Hostname, row.CheckedAt, want.CheckedAt)
		}
		if row.Manager != want.Manager || row.Error != want.Error {
			t.Errorf("%s: manager %q error %q, want %q %q", row.Hostname, row.Manager, row.Error, want.Manager, want.Error)
		}
		if row.Updates != len(want.Updates) || row.Security != want.securityCount() || row.RebootRequired != want.RebootRequired {
			t.Errorf("%s: updates %d security %d reboot %v, want %d %d %v (sent %+v)", row.Hostname,
				row.Updates, row.Security, row.RebootRequired,
				len(want.Updates), want.securityCount(), want.RebootRequired, sent)
		}
	}
	if seen != len(reports) {
		t.Fatalf("fleet view showed %d of the %d servers that reported", seen, len(reports))
	}
}

func TestAPackageSummaryWithNoUsableTimeIsNoReport(t *testing.T) {
	bad := "yesterday"
	var row PackageFleetRow
	packageSummary{checkedAt: &bad, updates: 3}.apply(&row)
	if row.CheckedAt != nil || row.Updates != 0 {
		t.Fatalf("a report that would not decode filled the row: %+v", row)
	}
}
