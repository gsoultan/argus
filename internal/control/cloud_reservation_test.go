package control

import (
	"encoding/json"
	"net/http"
	"strings"
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

// Reserving capacity and cancelling it go through the usual change path;
// a launch can land in it only where it keeps room.
func TestCloudCapacityReservations(t *testing.T) {
	e := changeAPI(t)
	region := "ap-southeast-3"
	body := func(action, extra string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + region + `","action":"` + action + `"` + extra + `}`
	}
	e.ask(t, "admin-token", body("capacity-create", `,"spec":{"zone":"us-east-1a","size":"m6i.large","count":2}`), http.StatusBadRequest)
	e.ask(t, "admin-token", body("capacity-create", `,"spec":{"zone":"ap-southeast-3a","size":"m6i.large","count":0}`), http.StatusBadRequest)
	// Production: it waits for a second person, then runs as the one who asked.
	c := e.ask(t, "admin-token", body("capacity-create", `,"reason":"sale","spec":{"zone":"ap-southeast-3a","size":"m6i.large","count":2,"targeted":true,"name":"sale-room"}`), http.StatusAccepted)
	if c.State != "awaiting_approval" || !strings.Contains(c.Summary, "Reserve room for 2 m6i.large in ap-southeast-3a for launches that name it, until cancelled") {
		t.Fatalf("create: %+v", c)
	}
	done := e.decide(t, "approver-token", c.ID, true)
	if done.State != "applied" || !strings.HasPrefix(done.ResourceID, "cr-fake") {
		t.Fatalf("applied: %+v", done)
	}
	e.resync(t)
	cr := done.ResourceID

	launch := func(subnet, size string, want int) CloudChange {
		spec := `{"name":"sale-01","image":"ami-ubuntu","size":"` + size + `","subnet":"` + subnet + `","firewalls":["` + e.account +
			`-sg-1"],"capacityReservation":"` + cr + `","count":2,"tags":{"Environment":"development"}}`
		return e.ask(t, "admin-token", body("launch", `,"spec":`+spec), want)
	}
	launch(e.account+"-subnet-1", "m6i.xlarge", http.StatusConflict)
	l := launch(e.account+"-subnet-1", "m6i.large", http.StatusOK)
	if l.State != "applied" || !strings.Contains(l.Summary, "into capacity reservation sale-room") || e.fake.LastLaunch.CapacityReservation != cr {
		t.Fatalf("launch into it: %+v %+v", l, e.fake.LastLaunch)
	}

	x := e.ask(t, "admin-token", body("capacity-cancel", `,"resourceId":"`+cr+`","reason":"sale over"`), http.StatusAccepted)
	if !strings.Contains(x.Summary, "Cancel sale-room (room for 2 m6i.large in ap-southeast-3a)") {
		t.Fatalf("cancel: %+v", x)
	}
	if d := e.decide(t, "approver-token", x.ID, true); d.State != "applied" {
		t.Fatalf("cancelled: %+v", d)
	}
}

func (e changeEnv) decide(t *testing.T, token, id string, approve bool) CloudChange {
	t.Helper()
	verb := "decline"
	if approve {
		verb = "approve"
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+id+"/"+verb, token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", verb, w.Code, w.Body)
	}
	var c CloudChange
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}
