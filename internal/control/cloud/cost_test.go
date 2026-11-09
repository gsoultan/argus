package cloud

import (
	"testing"
	"time"
)

func TestSumCosts(t *testing.T) {
	now := time.Date(2026, 3, 31, 15, 0, 0, 0, time.UTC)
	rows := []CostRow{
		{Date: "2026-01-31", Service: "EC2", Amount: 99}, // before last month: ignored
		{Date: "2026-02-01", Service: "EC2", Amount: 10},
		{Date: "2026-02-28", Service: "S3", Amount: 5},
		{Date: "2026-03-01", Service: "EC2", Amount: 12.344},
		{Date: "2026-03-31", Service: "S3", Amount: 1},
		{Date: "2026-03-02", Service: "Tax", Amount: 0},
	}
	f := 1.0 // below the spend: lifted to it
	c := SumCosts(now, "USD", rows, &f)
	if c.Month != "2026-03" || c.MonthToDate != 13.34 || c.LastMonth != 15 {
		t.Fatalf("totals: %+v", c)
	}
	// The 31st of March compares with the whole of February.
	if c.LastMonthToDate != 15 {
		t.Fatalf("last month to date: %v", c.LastMonthToDate)
	}
	if c.Forecast == nil || *c.Forecast != 13.34 {
		t.Fatalf("forecast: %v", c.Forecast)
	}
	if len(c.Days) != 28+31 || c.Days[0].Date != "2026-02-01" || c.Days[len(c.Days)-1].Amount != 1 {
		t.Fatalf("days: %d %+v", len(c.Days), c.Days[len(c.Days)-1])
	}
	if len(c.Services) != 2 || c.Services[0].Service != "EC2" || c.Services[0].LastMonth != 10 || c.Services[1].ThisMonth != 1 {
		t.Fatalf("services: %+v", c.Services)
	}

	// The 10th compares with February 1 to 10, ten days each.
	mid := SumCosts(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "USD",
		[]CostRow{{Date: "2026-02-10", Amount: 1}, {Date: "2026-02-11", Amount: 2}}, nil)
	if mid.LastMonthToDate != 1 || mid.LastMonth != 3 || mid.Forecast != nil {
		t.Fatalf("mid-month: %+v", mid)
	}
}

func TestVolumeMonthly(t *testing.T) {
	for size, want := range map[string]float64{"100 GiB gp3": 8, "100 GiB gp2": 10, "8 GiB io2": 1, "10 GiB weird": 0.8} {
		if got, ok := VolumeMonthly(size); !ok || got != want {
			t.Errorf("%s: %v %v", size, got, ok)
		}
	}
	if _, ok := VolumeMonthly("big"); ok {
		t.Error("read a size that has none")
	}
}

func TestIdle(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-10 * 24 * time.Hour)
	old := now.Add(-100 * 24 * time.Hour)
	rs := []Resource{
		{Kind: KindVolume, ID: "vol-in-use", State: "in-use", Size: "8 GiB gp3"},
		{Kind: KindAddress, ID: "eip-on", State: "attached"},
		// Stopped, but not for long enough.
		{Kind: KindInstance, ID: "i-new", State: "stopped", Details: map[string]string{"stoppedAt": recent.Format(time.RFC3339)}},
		// Stopped with no time said: left alone.
		{Kind: KindInstance, ID: "i-unknown", State: "stopped"},
		// An old snapshot an image uses, and one of a disk still there.
		{Kind: KindSnapshot, ID: "snap-image", Size: "8 GiB", CreatedAt: &old},
		{Kind: KindImage, ID: "ami-1", Links: []Ref{{Kind: KindSnapshot, ID: "snap-image"}}},
		{Kind: KindSnapshot, ID: "snap-kept", Size: "8 GiB", CreatedAt: &old, Links: []Ref{{Kind: KindVolume, ID: "vol-in-use"}}},
		// Load balancers: one serving, one whose groups are empty, one with none.
		{Kind: KindLoadBalancer, ID: "lb-live", State: "active"},
		{Kind: KindLoadBalancer, ID: "lb-empty", State: "active"},
		{Kind: KindLoadBalancer, ID: "lb-bare", State: "active"},
		{Kind: KindLoadBalancer, ID: "lb-starting", State: "provisioning"},
		{Kind: KindTargetGroup, ID: "tg-1", State: "healthy", Links: []Ref{{Kind: KindLoadBalancer, ID: "lb-live"}}},
		{Kind: KindTargetGroup, ID: "tg-2", State: "empty", Links: []Ref{{Kind: KindLoadBalancer, ID: "lb-empty"}}},
	}
	got := Idle(rs, now)
	if len(got) != 2 || got[0].ID != "lb-bare" || got[1].ID != "lb-empty" || got[1].Why != "Its target groups have no targets" ||
		got[0].Monthly != 16.43 || got[0].Fix != "" {
		t.Fatalf("idle: %+v", got)
	}
}
