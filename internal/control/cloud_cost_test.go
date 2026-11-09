package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 6: costs, idle resources and budgets, against the fake
provider.
*/

func (e changeEnv) costRows(now time.Time, perDay float64) {
	var rows []cloud.CostRow
	start := cloud.MonthStart(now).AddDate(0, -1, 0)
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		rows = append(rows, cloud.CostRow{Date: d.Format(time.DateOnly), Service: "Amazon Elastic Compute Cloud - Compute", Amount: perDay})
	}
	e.fake.CostRows = rows
}

func (e changeEnv) cleanCosts(t *testing.T) {
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM console_alerts WHERE subject IN
			(SELECT 'cloud-budget:' || id::text FROM cloud_budgets WHERE account_id::text = $1)`, e.acct.ID)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_budgets WHERE account_id::text = $1`, e.acct.ID)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_costs WHERE account_id::text = $1`, e.acct.ID)
	})
}

func (e changeEnv) costsView(t *testing.T) (CloudCostRead, []cloudBudgetView) {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/costs", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("costs: %d %s", w.Code, w.Body)
	}
	var v struct {
		Accounts []CloudCostRead   `json:"accounts"`
		Budgets  []cloudBudgetView `json:"budgets"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	var mine []cloudBudgetView
	for _, b := range v.Budgets {
		if b.AccountID == e.acct.ID {
			mine = append(mine, b)
		}
	}
	for _, a := range v.Accounts {
		if a.AccountID == e.acct.ID {
			return a, mine
		}
	}
	t.Fatalf("account not in the costs view: %s", w.Body)
	return CloudCostRead{}, nil
}

func budgetAlerts(t *testing.T, s *Store, id string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT severity || ' ' || title FROM console_alerts WHERE subject = $1 ORDER BY seq`,
		"cloud-budget:"+id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestCloudCostsReadAndAskAgain(t *testing.T) {
	e := changeAPI(t)
	e.cleanCosts(t)
	now := time.Now().UTC()
	e.costRows(now, 10)

	// Nothing read yet: the account is listed with no costs.
	if r, _ := e.costsView(t); r.Costs != nil {
		t.Fatalf("read before reading: %+v", r)
	}
	// Only administrators ask for a read; once in a while.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/costs/"+e.acct.ID+"/read", "approver-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("approver read: %d", w.Code)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/costs/"+e.acct.ID+"/read", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	var r CloudCostRead
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.Costs == nil || r.Costs.MonthToDate != float64(10*now.Day()) || r.ReadAt == nil {
		t.Fatalf("costs: %+v", r.Costs)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/costs/"+e.acct.ID+"/read", "admin-token", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second read: %d %s", w.Code, w.Body)
	}

	// A failed read keeps the last good one and says why.
	e.fake.CostErr = cloud.ErrCostsDenied
	c, _ := e.s.CloudAccount(context.Background(), e.acct.ID)
	if err := e.a.readCloudCosts(context.Background(), c, now); !errors.Is(err, cloud.ErrCostsDenied) {
		t.Fatalf("denied: %v", err)
	}
	r, _ = e.costsView(t)
	if r.Costs == nil || !strings.Contains(r.Error, "argus-access stack") {
		t.Fatalf("after a failed read: %+v", r)
	}

	// The background read leaves an account read recently alone.
	e.fake.CostErr = nil
	calls := len(e.fake.CallLog())
	e.a.readDueCloudCosts(context.Background(), now)
	for _, c := range e.fake.CallLog()[calls:] {
		if c == "costs "+e.account {
			t.Fatal("read again before it was due")
		}
	}
	e.a.readDueCloudCosts(context.Background(), now.Add(CloudCostInterval+time.Minute))
	read := false
	for _, c := range e.fake.CallLog()[calls:] {
		read = read || c == "costs "+e.account
	}
	if !read {
		t.Fatal("not read once due")
	}
}

func TestCloudBudgetAlertsOnceAMonth(t *testing.T) {
	e := changeAPI(t)
	e.cleanCosts(t)
	now := time.Now().UTC()
	e.costRows(now, 10)
	c, _ := e.s.CloudAccount(context.Background(), e.acct.ID)
	if err := e.a.readCloudCosts(context.Background(), c, now); err != nil {
		t.Fatal(err)
	}
	spent := float64(10 * now.Day())

	// Validation.
	for _, body := range []string{`{"name":"","amount":100}`, `{"name":"x","amount":0}`, `{"name":"x","amount":10,"thresholds":[0]}`,
		`{"name":"x","amount":10,"thresholds":[10,20,30,40,50,60]}`, `{"name":"x","amount":10,"accountId":"nope"}`} {
		if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/budgets", "admin-token", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/budgets", "approver-token", `{"name":"x","amount":10}`); w.Code != http.StatusForbidden {
		t.Fatalf("approver budget: %d", w.Code)
	}

	// A budget the spend is already past alerts at once, for the highest
	// threshold passed only.
	body, _ := json.Marshal(map[string]any{"accountId": e.acct.ID, "name": "payments", "amount": spent / 1.2, "forecast": true})
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/budgets", "admin-token", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("budget: %d %s", w.Code, w.Body)
	}
	var b CloudBudget
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if got := budgetAlerts(t, e.s, b.ID); len(got) != 1 || !strings.HasPrefix(got[0], `critical Budget "payments" is at 1`) {
		t.Fatalf("alerts: %v", got)
	}
	// Checking again sends nothing new.
	e.a.checkCloudBudgets(context.Background(), now)
	if got := budgetAlerts(t, e.s, b.ID); len(got) != 1 {
		t.Fatalf("alerted twice: %v", got)
	}
	_, views := e.costsView(t)
	if len(views) != 1 || views[0].Spent != spent || views[0].AccountName != e.acct.Name {
		t.Fatalf("standing: %+v", views)
	}

	// Raised above the spend but under the forecast: the forecast alerts.
	f := spent * 3
	e.fake.CostForecast = &f
	if err := e.a.readCloudCosts(context.Background(), c, now); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"accountId": e.acct.ID, "name": "payments", "amount": spent * 2, "forecast": true})
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/budgets/"+b.ID, "admin-token", string(body)); w.Code != http.StatusOK {
		t.Fatalf("change: %d %s", w.Code, w.Body)
	}
	got := budgetAlerts(t, e.s, b.ID)
	if len(got) != 2 || !strings.Contains(got[1], "forecast to run over") {
		t.Fatalf("forecast alert: %v", got)
	}

	// Removing it closes its alerts.
	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/budgets/"+b.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	var open int
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM console_alerts WHERE subject = $1 AND closed = ''`,
		"cloud-budget:"+b.ID).Scan(&open)
	if open != 0 {
		t.Fatalf("%d alerts still open", open)
	}
}

func TestCloudIdleResources(t *testing.T) {
	e := changeAPI(t)
	region := e.acct.Regions[0]
	old := time.Now().Add(-200 * 24 * time.Hour).UTC()
	stopped := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	e.fake.Resources[region] = append(e.fake.Resources[region],
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-spare", Name: "spare", Region: region, State: "available", Size: "500 GiB gp3"},
		cloud.Resource{Kind: cloud.KindAddress, ID: e.account + "-eip-1", Name: "1.2.3.4", Region: region, State: "unattached"},
		cloud.Resource{Kind: cloud.KindInstance, ID: e.account + "-i-old", Name: "old-batch", Region: region, State: "stopped",
			Details: map[string]string{"stoppedAt": stopped}, Links: []cloud.Ref{{Kind: cloud.KindVolume, ID: e.account + "-vol-old"}}},
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-old", Name: "old-root", Region: region, State: "in-use", Size: "100 GiB gp2"},
		cloud.Resource{Kind: cloud.KindSnapshot, ID: e.account + "-snap-1", Name: "orphan", Region: region, State: "completed",
			Size: "100 GiB", CreatedAt: &old, Links: []cloud.Ref{{Kind: cloud.KindVolume, ID: "vol-long-gone"}}},
	)
	e.resync(t)
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/idle", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("idle: %d %s", w.Code, w.Body)
	}
	var v struct {
		Items []cloudIdleItem `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	got := map[string]cloudIdleItem{}
	for _, it := range v.Items {
		if it.AccountID == e.acct.ID {
			got[it.ID] = it
		}
	}
	want := map[string]float64{e.account + "-vol-spare": 40, e.account + "-eip-1": 3.65, e.account + "-i-old": 10, e.account + "-snap-1": 5}
	if len(got) != len(want) {
		t.Fatalf("idle: %+v", got)
	}
	for id, m := range want {
		if got[id].Monthly != m {
			t.Errorf("%s: %v a month, want %v", id, got[id].Monthly, m)
		}
	}
	if got[e.account+"-vol-spare"].Fix != cloud.ActionVolumeDelete || got[e.account+"-i-old"].Fix != "" || !got[e.account+"-snap-1"].AtMost {
		t.Fatalf("fixes: %+v", got)
	}
}
