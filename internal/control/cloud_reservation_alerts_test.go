package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestReservationEnding(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for days, want := range map[float64]int{-1: 0, 0.5: 7, 7: 7, 7.1: 30, 30: 30, 31: 0} {
		if got := reservationEnding(now.Add(time.Duration(days*24)*time.Hour), now); got != want {
			t.Fatalf("%v days: %d", days, got)
		}
	}
}

// Ending reservations go to the bell once at 30 days and once at 7, and the
// 30-day one is not raised late.
func TestReservationAlerts(t *testing.T) {
	e := changeAPI(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ri := func(id string, ends time.Duration) cloud.Resource {
		return cloud.Resource{Kind: cloud.KindReservedInstance, ID: id, Name: "2 × m6i.large", Region: "ap-southeast-3", Size: "m6i.large", State: "active",
			Details: map[string]string{"count": "2", "scope": "region", "end": now.Add(ends).Format(time.RFC3339),
				"start": now.Add(-300 * 24 * time.Hour).Format(time.RFC3339)}}
	}
	e.fake.Resources[cloud.GlobalRegion] = append(e.fake.Resources[cloud.GlobalRegion],
		ri("ri-soon", 20*24*time.Hour), ri("ri-sooner", 3*24*time.Hour), ri("ri-later", 90*24*time.Hour))
	e.resync(t)
	count := func(like string) int {
		var n int
		_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE kind = 'cloud' AND subject LIKE $1`, like).Scan(&n)
		return n
	}
	e.a.watchReservations(ctx, now)
	e.a.watchReservations(ctx, now)
	if count("%ri-soon:30") != 1 || count("%ri-sooner:7") != 1 || count("%ri-sooner:30") != 0 || count("%ri-later%") != 0 {
		t.Fatalf("alerts: soon30=%d sooner7=%d sooner30=%d later=%d", count("%ri-soon:30"), count("%ri-sooner:7"), count("%ri-sooner:30"), count("%ri-later%"))
	}
	e.a.watchReservations(ctx, now.Add(14*24*time.Hour))
	if count("%ri-soon:7") != 1 {
		t.Fatal("no 7-day alert")
	}
	var title, sev, link string
	_ = e.s.pool.QueryRow(ctx, `SELECT title, severity, link FROM console_alerts WHERE kind = 'cloud' AND subject LIKE '%ri-sooner:7'`).Scan(&title, &sev, &link)
	if !strings.Contains(title, "Reserved Instance 2 × m6i.large in") || !strings.HasSuffix(title, "ends in 3 days") || sev != "warning" || link != "/cloud?tab=reservations" {
		t.Fatalf("alert: %q %s %s", title, sev, link)
	}
}
