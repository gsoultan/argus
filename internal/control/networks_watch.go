package control

import (
	"context"
	"strings"
	"time"
)

/*
The bell for Networks: an alert when a VPN link, cloud VPN or Cloudflare
tunnel goes down, closed with "Back up" when it comes back. Only what is
down is raised; what is only worth a look (one tunnel of two, a single
connector) stays on the page.

An alert's subject is "network:" and the problem's key, so each problem is
raised once while it lasts. A server whose agent has gone quiet is neither
raised nor closed: its links are not known.
*/

// AlertNetwork is a VPN link or tunnel going down or coming back.
const AlertNetwork = "network"

// NetworkWatchInterval is how often the links are judged.
const NetworkWatchInterval = time.Minute

const maxNetworkAlertsPerSweep = 25

// RunNetworkWatch raises and closes network alerts for each organization
// until ctx ends.
func (a *API) RunNetworkWatch(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().UTC()
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.watchNetworks(tctx, now)
		})
		if err != nil {
			a.log.Error("network watch", "error", err)
		}
		t.Reset(interval)
	}
}

func networkSubject(key string) string { return "network:" + key }

// keyAsset is the asset a problem key names, or "".
func keyAsset(key string) string {
	_, rest, _ := strings.Cut(key, ":")
	asset, _, _ := strings.Cut(rest, ":")
	return asset
}

func (a *API) watchNetworks(ctx context.Context, now time.Time) {
	v, err := a.store.Networks(ctx, now)
	if err != nil {
		a.log.Error("network watch: cannot read networks", "error", err)
		return
	}
	open, err := a.store.openNetworkAlerts(ctx)
	if err != nil {
		a.log.Error("network watch: cannot read open alerts", "error", err)
		return
	}
	raise, closing := networkAlertChanges(v, open)
	for _, p := range raise {
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertNetwork, Severity: "critical", Audience: AudienceAdmins,
			Title: p.Title, Detail: p.Detail, Link: "/networks", Subject: networkSubject(p.Key)})
	}
	for _, s := range closing {
		a.closeAlerts(ctx, s, "Back up")
	}
}

// networkAlertChanges is which problems to raise and which open alerts to
// close. Apart from the database, so it is tested without one.
func networkAlertChanges(v NetworksView, open map[string]bool) (raise []NetworkProblem, closing []string) {
	down := map[string]bool{}
	for _, p := range v.Problems {
		if p.Severity != "down" || p.Key == "" {
			continue
		}
		s := networkSubject(p.Key)
		down[s] = true
		if !open[s] && len(raise) < maxNetworkAlertsPerSweep {
			raise = append(raise, p)
		}
	}
	for s := range open {
		if down[s] || v.quiet[keyAsset(strings.TrimPrefix(s, "network:"))] {
			continue
		}
		closing = append(closing, s)
	}
	return raise, closing
}

// openNetworkAlerts are the subjects of network alerts not yet closed.
func (s *Store) openNetworkAlerts(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db(ctx).Query(ctx,
		`SELECT DISTINCT subject FROM console_alerts WHERE kind = $1 AND closed_at IS NULL AND subject LIKE 'network:%'`, AlertNetwork)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			return nil, err
		}
		out[sub] = true
	}
	return out, rows.Err()
}
