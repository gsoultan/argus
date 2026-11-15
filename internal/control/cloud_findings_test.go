package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Risks & waste, against the fake provider: what is listed, keeping a
finding with a reason and an audit entry, and what keeping may not do.
*/

type findingsView struct {
	Items    []cloudFindingItem     `json:"items"`
	Kept     []cloudFindingItem     `json:"kept"`
	Accounts []cloudFindingsAccount `json:"accounts"`
	Monthly  float64                `json:"monthly"`
}

func (e changeEnv) findings(t *testing.T, token string) findingsView {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/findings", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("findings: %d %s", w.Code, w.Body)
	}
	var v findingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v findingsView) mine(acct string) map[string]cloudFindingItem {
	out := map[string]cloudFindingItem{}
	for _, it := range v.Items {
		if it.AccountID == acct {
			out[it.ID] = it
		}
	}
	return out
}

func TestCloudFindings(t *testing.T) {
	e := changeAPI(t)
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM cloud_finding_keeps WHERE account_id::text = $1`, e.acct.ID)
	})
	region := e.acct.Regions[0]
	sg := e.account + "-sg-open"
	e.fake.Resources[region] = append(e.fake.Resources[region],
		cloud.Resource{Kind: cloud.KindFirewall, ID: sg, Name: "sg-open", Region: region, Network: "vpc-1",
			Details: map[string]string{"interfaces": "1"},
			Rules:   []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"}}},
		cloud.Resource{Kind: cloud.KindFirewall, ID: e.account + "-sg-old", Name: "sg-old", Region: region, Network: "vpc-1",
			Details: map[string]string{"interfaces": "0"}},
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-spare", Name: "spare", Region: region, State: "available", Size: "100 GiB gp3",
			Details: map[string]string{"encrypted": "yes"}},
	)
	e.fake.Resources[cloud.GlobalRegion] = []cloud.Resource{
		{Kind: cloud.KindAccount, ID: e.account, Name: "Root user", Region: cloud.GlobalRegion, Details: map[string]string{"rootMfa": "no", "rootKeys": "0"}},
	}
	e.resync(t)

	v := e.findings(t, "auditor-token")
	got := v.mine(e.acct.ID)
	open := "open:" + sg + ":tcp/22-22/0.0.0.0/0"
	for _, id := range []string{open, "group-unused:" + e.account + "-sg-old", "idle-volume:" + e.account + "-vol-spare", "root-no-mfa:" + e.account} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing %s in %v", id, findingIDs(got))
		}
	}
	if got[open].Severity != cloud.SeverityHigh || got[open].Fix != cloud.ActionFirewallRules {
		t.Fatalf("open SSH: %+v", got[open])
	}
	checked := false
	for _, a := range v.Accounts {
		if a.ID == e.acct.ID {
			checked = a.AccountChecks
		}
	}
	if !checked {
		t.Fatal("the account-wide read was saved, so its checks were made")
	}

	keep := func(token, body string) int {
		return do(t, e.h, http.MethodPost, "/api/v1/cloud/findings/keep", token, body).Code
	}
	// Seeing Cloud is not enough to keep a finding.
	if c := keep("auditor-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"x","days":30}`); c != http.StatusForbidden {
		t.Fatalf("an auditor kept a finding: %d", c)
	}
	// A High risk is never kept for good.
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"bastion","untilChange":true}`); c != http.StatusBadRequest {
		t.Fatalf("a High risk was kept with no end: %d", c)
	}
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"","days":30}`); c != http.StatusBadRequest {
		t.Fatalf("kept without a reason: %d", c)
	}
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"x","days":45}`); c != http.StatusBadRequest {
		t.Fatalf("kept for a length that is not offered: %d", c)
	}
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"not now","days":7}`); c != http.StatusOK {
		t.Fatalf("keep for 7 days (Not now): %d", c)
	}
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"no-such","reason":"x","days":30}`); c != http.StatusNotFound {
		t.Fatalf("kept a finding that is not there: %d", c)
	}
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+open+`","reason":"bastion until the move","days":30}`); c != http.StatusOK {
		t.Fatalf("keep: %d", c)
	}
	var n int
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = 'cloud.finding.kept' AND detail LIKE '%bastion until the move%' AND target LIKE '%'||$1||'%'`, e.account).Scan(&n)
	if n != 1 {
		t.Fatalf("keep audit entries: %d", n)
	}
	v = e.findings(t, "auditor-token")
	if _, ok := v.mine(e.acct.ID)[open]; ok {
		t.Fatal("a kept finding is still listed")
	}
	var kept *cloudFindingItem
	for i, it := range v.Kept {
		if it.ID == open {
			kept = &v.Kept[i]
		}
	}
	if kept == nil || kept.Kept == nil || kept.Kept.Reason != "bastion until the move" {
		t.Fatalf("kept list: %+v", v.Kept)
	}

	// Kept until it changes: a new rule source is a new finding, the old
	// keep no longer hides anything.
	unused := "group-unused:" + e.account + "-sg-old"
	if c := keep("admin-token", `{"accountId":"`+e.acct.ID+`","findingId":"`+unused+`","reason":"template for the move","untilChange":true}`); c != http.StatusOK {
		t.Fatalf("keep until change: %d", c)
	}
	if _, ok := e.findings(t, "auditor-token").mine(e.acct.ID)[unused]; ok {
		t.Fatal("kept until it changes, and it has not")
	}
	_, _ = e.s.pool.Exec(context.Background(), `UPDATE cloud_finding_keeps SET basis = 'stale' WHERE finding_id = $1`, unused)
	if _, ok := e.findings(t, "auditor-token").mine(e.acct.ID)[unused]; !ok {
		t.Fatal("what the keep rested on changed, so the finding is back")
	}

	// Expired keeps hide nothing.
	_, _ = e.s.pool.Exec(context.Background(), `UPDATE cloud_finding_keeps SET expires_at = $2 WHERE finding_id = $1`, open, time.Now().Add(-time.Minute))
	if _, ok := e.findings(t, "auditor-token").mine(e.acct.ID)[open]; !ok {
		t.Fatal("an expired keep still hides the finding")
	}

	q := url.Values{"accountId": {e.acct.ID}, "findingId": {open}}
	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/findings/keep?"+q.Encode(), "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("unkeep: %d %s", w.Code, w.Body)
	}
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = 'cloud.finding.unkept' AND detail LIKE '%'||$1||'%'`, open).Scan(&n) // open names this test's account
	if n != 1 {
		t.Fatalf("unkeep audit entries: %d", n)
	}
}

func TestCloudFindingsAgreeWithIdle(t *testing.T) {
	e := changeAPI(t)
	region := e.acct.Regions[0]
	e.fake.Resources[region] = append(e.fake.Resources[region],
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-spare", Name: "spare", Region: region, State: "available", Size: "100 GiB gp3"},
		cloud.Resource{Kind: cloud.KindAddress, ID: e.account + "-eip-1", Name: "1.2.3.4", Region: region, State: "unattached"},
	)
	e.resync(t)
	var idle struct {
		Monthly float64 `json:"monthly"`
	}
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/idle", "admin-token", "")
	_ = json.Unmarshal(w.Body.Bytes(), &idle)
	if v := e.findings(t, "admin-token"); v.Monthly != idle.Monthly {
		t.Fatalf("Risks & waste says %v a month, Costs %v", v.Monthly, idle.Monthly)
	}
}

func findingIDs[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
