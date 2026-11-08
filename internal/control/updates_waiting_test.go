package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

/*
What is waiting to be updated, across the fleet: the list the Updates page
and the Needs updates filter read, who may read it, and the bell's digest,
once a day per kind.
*/

func TestUpdatesWaitingAndTheDailyDigest(t *testing.T) {
	e := packagesAPI(t)
	ctx := context.Background()
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM console_alerts WHERE kind = 'updates'`)

	sec, secCred := e.server(t)
	plain, plainCred := e.server(t)
	clean, cleanCred := e.server(t)
	now := time.Now().UTC()
	e.beat(t, secCred, sec.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now,
		PackageUpdate{Name: "openssl", Available: "3.0.13-2", Security: true},
		PackageUpdate{Name: "curl", Available: "8.5.0-2"})})
	plainReport := pkgReport(now, PackageUpdate{Name: "vim", Available: "9.1"})
	plainReport.RebootRequired = true
	e.beat(t, plainCred, plain.Hostname, &PackageBeat{Supported: true, Report: plainReport})
	e.beat(t, cleanCred, clean.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now)})

	w := do(t, e.h, http.MethodGet, "/api/v1/updates-waiting", "auditor-token", "")
	var v UpdatesWaiting
	if _ = json.Unmarshal(w.Body.Bytes(), &v); w.Code != http.StatusOK {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	got := map[string]UpdatesWaitingRow{}
	for _, r := range v.Assets {
		got[r.AssetID] = r
	}
	if r := got[sec.ID]; r.Security != 1 || r.Updates != 2 {
		t.Errorf("security server: %+v", r)
	}
	if r := got[plain.ID]; r.Security != 0 || r.Updates != 1 || !r.RebootRequired {
		t.Errorf("plain server: %+v", r)
	}
	if _, ok := got[clean.ID]; ok {
		t.Error("a server with nothing waiting is listed")
	}
	// Security first.
	if len(v.Assets) == 0 || v.Assets[0].Security == 0 {
		t.Errorf("order: %+v", v.Assets)
	}
	if v.SecurityAssets < 1 || v.RebootAssets < 1 {
		t.Errorf("counts: %+v", v)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/updates-waiting", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read it: %d", w.Code)
	}

	count := func(subject string) int {
		var n int
		_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE subject = $1`, subject).Scan(&n)
		return n
	}
	day := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	e.a.digestUpdates(ctx, day)
	e.a.digestUpdates(ctx, day.Add(time.Hour))
	if n := count("updates:security:2026-11-08"); n != 1 {
		t.Errorf("security digests today: %d, want 1", n)
	}
	if n := count("updates:restart:2026-11-08"); n != 1 {
		t.Errorf("restart digests today: %d, want 1", n)
	}
	e.a.digestUpdates(ctx, day.Add(24*time.Hour))
	if n := count("updates:security:2026-11-09"); n != 1 {
		t.Errorf("the next day's digest: %d", n)
	}
}
