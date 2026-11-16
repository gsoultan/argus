package control

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// Reservations from the account-wide read: listed with their use, the
// coverage of the running instances, which parts could be read, and an
// unused one among Risks & waste.
func TestCloudReservations(t *testing.T) {
	e := changeAPI(t)
	region := "ap-southeast-3" // where pay-api-01 runs
	start := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	ri := func(id, size string) cloud.Resource {
		return cloud.Resource{Kind: cloud.KindReservedInstance, ID: e.account + "-" + id, Name: "1 × " + size, Region: region, Size: size, State: "active",
			Details: map[string]string{"count": "1", "scope": "region", "platform": "Linux/UNIX", "start": start, "hourly": "0.1", "termMonths": "12"}}
	}
	e.fake.Resources[cloud.GlobalRegion] = []cloud.Resource{
		ri("ri-used", "m6i.large"), ri("ri-idle", "c6i.large"),
		{Kind: cloud.KindReservationsRead, ID: "reservations", Region: cloud.GlobalRegion,
			Details: map[string]string{"reservedInstances": "yes", "capacityReservations": "yes", "savingsPlans": "no", "savingsPlansWhy": "update"}},
	}
	e.resync(t)

	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/reservations", "auditor-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("reservations: %d %s", w.Code, w.Body)
	}
	var v struct {
		Accounts          []cloudReservationsAccount `json:"accounts"`
		ReservedInstances []cloudRIItem              `json:"reservedInstances"`
		Coverage          []cloudCoverageItem        `json:"coverage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	use := map[string]cloud.RIUse{}
	for _, it := range v.ReservedInstances {
		if it.AccountID == e.acct.ID {
			use[it.Resource.ID] = it.Use
		}
	}
	if u := use[e.account+"-ri-used"]; u.Use != 1 || len(u.Covers) != 1 {
		t.Fatalf("the m6i.large RI lands on the running m6i.xlarge: %+v", u)
	}
	if u := use[e.account+"-ri-idle"]; u.Used != 0 {
		t.Fatalf("nothing runs c6i: %+v", u)
	}
	var acc cloudReservationsAccount
	for _, a := range v.Accounts {
		if a.ID == e.acct.ID {
			acc = a
		}
	}
	if !acc.Read || !acc.ReservedInstances || acc.SavingsPlans || !acc.SavingsPlansNeedRole {
		t.Fatalf("what was read: %+v", acc)
	}
	var cov *cloudCoverageItem
	for i := range v.Coverage {
		if v.Coverage[i].Account == e.acct.ID {
			cov = &v.Coverage[i]
		}
	}
	if cov == nil || cov.ByRI <= 0 || cov.Rest <= 0 || cov.AccountName == "" {
		t.Fatalf("half the m6i.xlarge is covered: %+v", cov)
	}
	got := e.findings(t, "auditor-token").mine(e.acct.ID)
	if f, ok := got["reservation-unused:"+e.account+"-ri-idle"]; !ok || f.Monthly != 73 {
		t.Fatalf("the idle RI is waste, $73 a month: %+v in %v", f, findingIDs(got))
	}
	if _, ok := got["reservation-unused:"+e.account+"-ri-used"]; ok {
		t.Fatal("a used RI is not waste")
	}
}
