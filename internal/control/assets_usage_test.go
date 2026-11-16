package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

/*
The Assets cards' usage row: every role gets it, but only for the assets it
can see on that page. A member or approver sees the servers given to them,
by name or through a team, and never a server they were not given, which is
the refusal that matters here.
*/

func TestAssetsUsageIsScopedToWhatEachRoleSees(t *testing.T) {
	a, s, h, admin := enrollAPI(t)
	ctx := context.Background()
	member, _ := newAccount(t, s, "operator")
	teamMate, _ := newAccount(t, s, "operator")
	approver, _ := newAccount(t, s, "approver")
	viewer, _ := newAccount(t, s, "auditor")
	a.UserTokens["member-token"] = member
	a.UserTokens["team-token"] = teamMate
	a.UserTokens["approver-token"] = approver
	a.UserTokens["viewer-token"] = viewer

	mine, mineCred := enrolledAsset(t, s, h)
	other, otherCred := enrolledAsset(t, s, h)
	now := time.Now().UTC()
	beat := func(host, cred string, busy float64, memAvail int, used, avail int) {
		t.Helper()
		body := fmt.Sprintf(`{"hostname":%q,"version":"0.9.9","usage":{"at":%q,"cpu_busy":%g,`+
			`"mem_total":1000,"mem_available":%d,"swap_total":0,"swap_free":0,`+
			`"disks":[{"mount":"/","fs":"ext4","total":100,"used":10,"avail":90},`+
			`{"mount":"/data","fs":"xfs","total":100,"used":%d,"avail":%d}]}}`,
			host, now.Format(time.RFC3339), busy, memAvail, used, avail)
		if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
	}
	beat(mine.Hostname, mineCred, 18.4, 390, 91, 9) // memory 61% used, /data 91% full
	beat(other.Hostname, otherCred, 50, 500, 20, 80)

	if err := s.SetAssignment(ctx, mine.ID, member, []string{"ops"}, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAssignment(ctx, mine.ID, approver, []string{"ops"}, admin); err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, unique("team"), []string{teamMate}, admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamAssignment(ctx, mine.ID, team, []string{"ops"}, admin); err != nil {
		t.Fatal(err)
	}

	get := func(token string) map[string]AssetUsageRow {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/v1/assets-usage", token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", token, w.Code, w.Body)
		}
		var v AssetsUsage
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		out := map[string]AssetUsageRow{}
		for _, r := range v.Assets {
			out[r.AssetID] = r
		}
		return out
	}

	for _, tok := range []string{"admin-token", "viewer-token"} {
		got := get(tok)
		if _, ok := got[mine.ID]; !ok {
			t.Errorf("%s: missing an asset it sees on Assets", tok)
		}
		if _, ok := got[other.ID]; !ok {
			t.Errorf("%s: missing an asset it sees on Assets", tok)
		}
	}
	for _, tok := range []string{"member-token", "team-token", "approver-token"} {
		got := get(tok)
		if _, ok := got[other.ID]; ok {
			t.Errorf("%s read a server it was not given", tok)
		}
		r, ok := got[mine.ID]
		if !ok {
			t.Fatalf("%s: missing the server it was given", tok)
		}
		if r.CPU == nil || *r.CPU != 18 || r.Memory == nil || *r.Memory != 61 ||
			r.Disk == nil || r.Disk.Mount != "/data" || r.Disk.Percent != 91 {
			t.Errorf("%s: figures = %+v, want cpu 18, memory 61, /data 91", tok, r)
		}
		if len(r.Warnings) != 1 || r.Warnings[0].Resource != ResourceDisk || r.Warnings[0].Mount != "/data" {
			t.Errorf("%s: warnings = %+v, want /data running low", tok, r.Warnings)
		}
	}

	// Taken off the team, the server goes with it.
	if err := s.RemoveTeamMember(ctx, team, teamMate); err != nil {
		t.Fatal(err)
	}
	if _, ok := get("team-token")[mine.ID]; ok {
		t.Error("a member removed from the team still read the team's server")
	}
}

func TestAssetsUsageLeavesOutStaleReports(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	old := time.Now().UTC().Add(-2 * resourcesFresh)
	body := fmt.Sprintf(`{"hostname":%q,"version":"0.9.9","usage":{"at":%q,"cpu_busy":5,`+
		`"mem_total":1000,"mem_available":900,"swap_total":0,"swap_free":0,"disks":[]}}`,
		asset.Hostname, old.Format(time.RFC3339))
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodGet, "/api/v1/assets-usage", "admin-token", "")
	var v AssetsUsage
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	for _, r := range v.Assets {
		if r.AssetID == asset.ID {
			t.Errorf("a report from %s ago was shown as now: %+v", 2*resourcesFresh, r)
		}
	}
}

func TestAssetsUsageNeedsSignIn(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	if w := do(t, h, http.MethodGet, "/api/v1/assets-usage", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", w.Code)
	}
}

func TestUsageFigures(t *testing.T) {
	busy := 99.6
	cpu, mem, disk := usageFigures(HostUsage{CPUBusy: &busy, MemTotal: 0,
		Disks: []HostDisk{{Mount: "/", Used: 0, Avail: 0}, {Mount: "/x", Used: 3, Avail: 1}}})
	if cpu == nil || *cpu != 100 || mem != nil || disk == nil || disk.Mount != "/x" || disk.Percent != 75 {
		t.Errorf("got cpu %v mem %v disk %+v", cpu, mem, disk)
	}
}
