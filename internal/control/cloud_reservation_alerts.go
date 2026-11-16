package control

import (
	"context"
	"fmt"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Reservations ending soon, in the bell: once 30 days before a Reserved
Instance, Savings Plan or time-limited Capacity Reservation ends, and again
7 days before. Nothing in AWS renews them, so when one ends its instances
quietly go back to the on-demand rate; the bell says so while there is time
to buy again. Read from the inventory, hourly, so it costs AWS nothing.
*/

// reservationAlertDays are the days before an end the bell says so.
var reservationAlertDays = []int{7, 30}

// reservationEnding is which alert, if any, an end at end is due at now:
// the nearest threshold it is within, or 0.
func reservationEnding(end, now time.Time) int {
	left := end.Sub(now)
	if left <= 0 {
		return 0
	}
	for _, d := range reservationAlertDays {
		if left <= time.Duration(d)*24*time.Hour {
			return d
		}
	}
	return 0
}

func reservationAlertSubject(acct, id string, days int) string {
	return fmt.Sprintf("reservation-ending:%s/%s:%d", acct, id, days)
}

// watchReservations raises the alerts due for the organization on ctx.
func (a *API) watchReservations(ctx context.Context, now time.Time) {
	var items []CloudItem
	for _, k := range []cloud.Kind{cloud.KindReservedInstance, cloud.KindSavingsPlan, cloud.KindCapacityReservation} {
		got, err := a.store.CloudResources(ctx, k)
		if err != nil {
			a.log.Error("reservation watch: cannot read reservations", "error", err)
			return
		}
		items = append(items, got...)
	}
	if len(items) == 0 {
		return
	}
	sent := map[string]bool{}
	rows, err := a.store.db(ctx).Query(ctx,
		`SELECT subject FROM console_alerts WHERE kind = $1 AND at >= $2`, AlertCloud, now.Add(-40*24*time.Hour))
	if err != nil {
		a.log.Error("reservation watch: cannot read earlier alerts", "error", err)
		return
	}
	for rows.Next() {
		var s *string
		if rows.Scan(&s) == nil && s != nil {
			sent[*s] = true
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	for _, it := range items {
		r := it.Resource
		if r.State != "active" && r.State != "payment-pending" {
			continue
		}
		end, err := time.Parse(time.RFC3339, r.Details["end"])
		if err != nil {
			continue
		}
		days := reservationEnding(end, now)
		if days == 0 {
			continue
		}
		subject := reservationAlertSubject(it.AccountID, r.ID, days)
		// The 30-day alert is not raised late, after the 7-day one.
		if sent[subject] || (days == 30 && sent[reservationAlertSubject(it.AccountID, r.ID, 7)]) {
			continue
		}
		title, detail := reservationAlertText(it, end, now)
		sev := "info"
		if days == 7 {
			sev = "warning"
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertCloud, Severity: sev, Audience: AudienceAdmins,
			Title: title, Detail: detail, Link: "/cloud?tab=reservations", Subject: subject})
		sent[subject] = true
	}
}

func reservationAlertText(it CloudItem, end, now time.Time) (string, string) {
	r := it.Resource
	left := int(end.Sub(now).Hours()/24 + 0.999)
	when := fmt.Sprintf("in %d days", left)
	if left == 1 {
		when = "tomorrow"
	}
	date := end.UTC().Format("2 Jan 2006")
	switch r.Kind {
	case cloud.KindSavingsPlan:
		return fmt.Sprintf("%s in %s ends %s", r.Name, it.AccountName, when),
			fmt.Sprintf("It ends on %s and does not renew. From then what it covered is billed at the on-demand rate. Buy again from Cloud › Reservations.", date)
	case cloud.KindCapacityReservation:
		return fmt.Sprintf("Capacity reservation %s in %s ends %s", r.Name, it.AccountName, when),
			fmt.Sprintf("Its room for %s × %s in %s goes on %s; instances in it keep running.", r.Details["total"], r.Size, r.Zone, date)
	}
	return fmt.Sprintf("Reserved Instance %s in %s ends %s", r.Name, it.AccountName, when),
		fmt.Sprintf("It ends on %s and does not renew. From then the instances it covers in %s are billed at the on-demand rate. Buy again from Cloud › Reservations.", date, r.Region)
}
