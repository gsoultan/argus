package cloud

import (
	"math"
	"testing"
	"time"
)

func TestSizeUnits(t *testing.T) {
	for size, want := range map[string]float64{"t3.nano": 0.25, "m6i.large": 4, "m6i.xlarge": 8, "m6i.4xlarge": 32, "c7g.24xlarge": 192} {
		if got, ok := SizeUnits(size); !ok || got != want {
			t.Fatalf("%s: %v %v", size, got, ok)
		}
	}
	if _, ok := SizeUnits("m6i.metal"); ok {
		t.Fatal("metal has no factor")
	}
}

func reservationFixture(now time.Time) []Resource {
	start := now.Add(-72 * time.Hour).Format(time.RFC3339)
	inst := func(id, size, zone, state, platform string) Resource {
		d := map[string]string{}
		if platform != "" {
			d["platform"] = platform
		}
		return Resource{Kind: KindInstance, ID: id, Name: "web-" + id, Region: "us-east-1", Zone: zone, Size: size, State: state, Details: d}
	}
	ri := func(id, size, count, scope, zone, platform string) Resource {
		return Resource{Kind: KindReservedInstance, ID: id, Name: count + " × " + size, Region: "us-east-1", Zone: zone, Size: size, State: "active",
			Details: map[string]string{"count": count, "scope": scope, "platform": platform, "tenancy": "default", "start": start,
				"hourly": "0.06", "termMonths": "12"}}
	}
	return []Resource{
		inst("i-1", "m6i.large", "us-east-1a", "running", ""),
		inst("i-2", "m6i.xlarge", "us-east-1b", "running", "Linux/UNIX"),
		inst("i-3", "c6i.large", "us-east-1a", "stopped", ""),
		inst("i-4", "t3.micro", "us-east-1a", "running", "Windows"),
		ri("ri-a", "m6i.large", "2", "region", "", "Linux/UNIX"),
		ri("ri-b", "m6i.large", "2", "region", "", "Linux/UNIX (Amazon VPC)"),
		ri("ri-c", "c6i.large", "1", "zone", "us-east-1a", "Linux/UNIX"),
		ri("ri-d", "t3.micro", "1", "region", "", "Windows"),
		{Kind: KindCapacityReservation, ID: "cr-1", Name: "cr-1", Region: "us-east-1", Zone: "us-east-1a", Size: "m6i.large", State: "active",
			Details: map[string]string{"total": "2", "available": "1", "start": start}},
		{Kind: KindSavingsPlan, ID: "sp-ec2", Name: "EC2 Instance Savings Plan (r6i, us-east-1)", Region: GlobalRegion, State: "active",
			Details: map[string]string{"type": "EC2Instance", "family": "r6i", "planRegion": "us-east-1", "commitment": "0.5", "start": start}},
		{Kind: KindSavingsPlan, ID: "sp-compute", Name: "Compute Savings Plan", Region: GlobalRegion, State: "active",
			Details: map[string]string{"type": "Compute", "commitment": "1", "start": start}},
	}
}

// Zonal first, then regional; a regional Linux one bends across the sizes
// of its family, a Windows one fits its own type only.
func TestAnalyseReservations(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	var all []AccountResource
	for _, r := range reservationFixture(now) {
		all = append(all, AccountResource{Account: "a1", Resource: r})
	}
	rep := AnalyseReservations(all)
	use := map[string]RIUse{}
	for _, u := range rep.RIs {
		use[u.ID] = u
	}
	if u := use["ri-a"]; u.Units != 8 || u.Used != 8 || !u.Flexible {
		t.Fatalf("ri-a covers i-1 and half of i-2: %+v", u)
	}
	if u := use["ri-b"]; u.Used != 4 || u.Use != 0.5 {
		t.Fatalf("ri-b gets the other half of i-2: %+v", u)
	}
	if u := use["ri-c"]; u.Used != 0 {
		t.Fatalf("ri-c matches nothing running: %+v", u)
	}
	if u := use["ri-d"]; u.Use != 1 || u.Flexible {
		t.Fatalf("ri-d covers the Windows t3.micro whole: %+v", u)
	}
	if u := use["ri-a"]; math.Abs(u.Monthly-2*0.06*730) > 0.01 {
		t.Fatalf("monthly: %v", u.Monthly)
	}
	if len(rep.Coverage) != 1 {
		t.Fatalf("coverage: %+v", rep.Coverage)
	}
	c := rep.Coverage[0]
	if c.Instances != 3 || math.Abs(c.Hourly-(0.096+0.192+0.0104)) > 0.01 || c.Rest > 0.01 {
		t.Fatalf("all three running are covered: %+v", c)
	}
	plans := map[string]SPUse{}
	for _, p := range rep.Plans {
		plans[p.ID] = p
	}
	if plans["sp-ec2"].Eligible != 0 || !plans["sp-compute"].AlsoOther {
		t.Fatalf("plans: %+v", rep.Plans)
	}
}

func TestReservationFindings(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	got := map[string]Finding{}
	for _, f := range Findings(reservationFixture(now), nil, now) {
		got[f.ID] = f
	}
	if f, ok := got["reservation-unused:ri-b"]; !ok || f.Group != GroupWaste || math.Abs(f.Monthly-43.8) > 0.01 {
		t.Fatalf("ri-b half unused, $43.80 a month: %+v", f)
	}
	if f := got["reservation-unused:ri-c"]; f.Why == "" || f.Monthly != 43.8 {
		t.Fatalf("ri-c: %+v", f)
	}
	for _, id := range []string{"reservation-unused:ri-a", "reservation-unused:ri-d", "savings-plan-idle:sp-compute"} {
		if _, ok := got[id]; ok {
			t.Fatalf("%s should not be a finding", id)
		}
	}
	if f := got["capacity-unused:cr-1"]; math.Abs(f.Monthly-0.096*730) > 0.01 {
		t.Fatalf("one empty m6i.large slot: %+v", f)
	}
	if f := got["savings-plan-idle:sp-ec2"]; f.Monthly != 365 {
		t.Fatalf("r6i plan with nothing running: %+v", f)
	}
	// Nothing in a reservation's first day.
	fresh := reservationFixture(now)
	for i := range fresh {
		if fresh[i].Details["start"] != "" {
			fresh[i].Details["start"] = now.Add(-time.Hour).Format(time.RFC3339)
		}
	}
	for _, f := range Findings(fresh, nil, now) {
		if f.Check == "reservation-unused" || f.Check == "capacity-unused" || f.Check == "savings-plan-idle" {
			t.Fatalf("too early: %+v", f)
		}
	}
}
