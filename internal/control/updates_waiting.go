package control

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

/*
What is waiting to be updated across the fleet, in one place: operating
system updates and security fixes (package_updates.go), agents behind the
offered version (agent_fleet.go), and restarts an update left pending.

	GET /api/v1/updates-waiting    admins, owners, auditors

The assets list reads it for its Needs updates filter and badges, the
Updates page for its groups. Once a day per organization a digest per kind
goes to the bell (kind "updates"), naming how many assets, never one alert
per package.
*/

// UpdatesWaitingRow is one asset with something waiting.
type UpdatesWaitingRow struct {
	AssetID        string `json:"assetId"`
	Hostname       string `json:"hostname"`
	Updates        int    `json:"updates"`
	Security       int    `json:"security"`
	RebootRequired bool   `json:"rebootRequired"`
	// Agent is the agent's state against the offered version: behind,
	// updating, held, cannot, too_old; empty when current or not known.
	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
}

// UpdatesWaiting is the fleet's view.
type UpdatesWaiting struct {
	// Latest is the agent version offered; "" when none is.
	Latest string              `json:"latest"`
	Assets []UpdatesWaitingRow `json:"assets"`
	// Counts of assets.
	SecurityAssets int `json:"securityAssets"`
	SecurityFixes  int `json:"securityFixes"`
	UpdateAssets   int `json:"updateAssets"`
	RebootAssets   int `json:"rebootAssets"`
	AgentsBehind   int `json:"agentsBehind"`
}

func (a *API) updatesWaiting(ctx context.Context) (UpdatesWaiting, error) {
	out := UpdatesWaiting{Assets: []UpdatesWaitingRow{}}
	byID := map[string]*UpdatesWaitingRow{}
	row := func(id, host string) *UpdatesWaitingRow {
		if r, ok := byID[id]; ok {
			return r
		}
		r := &UpdatesWaitingRow{AssetID: id, Hostname: host}
		byID[id] = r
		return r
	}
	pkgs, err := a.store.PackageFleet(ctx)
	if err != nil {
		return out, err
	}
	for _, p := range pkgs {
		if !p.Reported || (p.Updates == 0 && p.Security == 0 && !p.RebootRequired) {
			continue
		}
		r := row(p.AssetID, p.Hostname)
		r.Updates, r.Security, r.RebootRequired = p.Updates, p.Security, p.RebootRequired
	}
	agents, err := a.agentFleetView(ctx)
	if err != nil {
		return out, err
	}
	out.Latest = agents.Latest
	for _, g := range agents.Servers {
		switch g.State {
		case agentBehind, agentUpdating, agentHeld, agentCannot, agentTooOld:
			r := row(g.AssetID, g.Hostname)
			r.Agent, r.AgentVersion = g.State, g.AgentVersion
		}
	}
	for _, r := range byID {
		out.Assets = append(out.Assets, *r)
		if r.Security > 0 {
			out.SecurityAssets++
			out.SecurityFixes += r.Security
		}
		if r.Updates > 0 {
			out.UpdateAssets++
		}
		if r.RebootRequired {
			out.RebootAssets++
		}
		if r.Agent == agentBehind {
			out.AgentsBehind++
		}
	}
	sort.Slice(out.Assets, func(i, j int) bool {
		a, b := out.Assets[i], out.Assets[j]
		if (a.Security > 0) != (b.Security > 0) {
			return a.Security > 0
		}
		return a.Hostname < b.Hostname
	})
	return out, nil
}

func (a *API) registerUpdatesWaitingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/updates-waiting", a.getUpdatesWaiting)
}

func (a *API) getUpdatesWaiting(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "what is waiting to be updated is for administrators, owners and auditors")
		return
	}
	v, err := a.updatesWaiting(r.Context())
	if err != nil {
		a.fail(w, "updates waiting", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

/* ── The daily digest ────────────────────────────────────────────────────── */

// UpdateDigestInterval is how often the digest looks; each kind goes out at
// most once a day, so looking hourly only makes the first one prompt.
const UpdateDigestInterval = time.Hour

// RunUpdateDigest sends each organization's digest until ctx ends.
func (a *API) RunUpdateDigest(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.digestUpdates(tctx, time.Now().UTC())
		})
		if err != nil {
			a.log.Error("update digest", "error", err)
		}
		t.Reset(interval)
	}
}

// digestUpdates raises today's digest for each kind with something waiting
// that has not had one today.
func (a *API) digestUpdates(ctx context.Context, now time.Time) {
	v, err := a.updatesWaiting(ctx)
	if err != nil {
		a.log.Error("update digest: cannot read what is waiting", "error", err)
		return
	}
	day := now.Format("2006-01-02")
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	send := func(kind, severity, title, detail string) {
		subject := "updates:" + kind + ":" + day
		var seen bool
		if err := a.store.db(ctx).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM console_alerts WHERE subject = $1)`, subject).Scan(&seen); err != nil || seen {
			return
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertUpdates, Severity: severity, Audience: AudienceAdmins,
			Title: title, Detail: detail, Link: "/assets/updates", Subject: subject})
	}
	if v.SecurityAssets > 0 {
		send("security", "warning",
			fmt.Sprintf("Security fixes waiting on %s", plural(v.SecurityAssets, "asset", "assets")),
			fmt.Sprintf("%s in all. Open Updates to install them.", plural(v.SecurityFixes, "security fix", "security fixes")))
	}
	if v.AgentsBehind > 0 && v.Latest != "" {
		send("agents", "info",
			fmt.Sprintf("Agent %s is out: %s behind", v.Latest, plural(v.AgentsBehind, "asset", "assets")),
			"Each agent updates once nobody is connected to its asset.")
	}
	if v.RebootAssets > 0 {
		send("restart", "info",
			plural(v.RebootAssets, "asset needs", "assets need")+" a restart to finish updating",
			"An update installed changes that take effect after a restart.")
	}
}
