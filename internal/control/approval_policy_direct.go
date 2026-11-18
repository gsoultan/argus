package control

import (
	"context"
	"strings"
)

/*
Acting directly where the approval policy allows it (approval_policy.go).

Requests filed in access_requests (a login, a database session, a restore in
place, a replica) are still filed: the request carries the reason and the
grant's expiry that everything downstream reads. When the policy lets the
person who asked act directly, the request is approved at once, as them, and
Activity records that nobody else agreed and which rule allowed it.

Everything else (cloud changes, schedules and access, server runs, statements
and network changes on a database) asks actsDirectly where it decides whether
to wait, and records the action with recordDirect once it is made.
*/

// directNote is the decision note of a request the policy approved.
const directNote = "Approved by the organization's approval policy: nobody else needed to agree"

// hostsEnv is the strictest environment among hosts, from each server's
// tags. "" when it cannot be read, which actsDirectly answers with a wait.
func (a *API) hostsEnv(ctx context.Context, hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	lower := make([]string, 0, len(hosts))
	for _, h := range hosts {
		lower = append(lower, strings.ToLower(h))
	}
	rows, err := a.store.db(ctx).Query(ctx, `SELECT tags FROM assets WHERE lower(hostname) = ANY($1)`, lower)
	if err != nil {
		a.log.Error("reading server tags for the approval policy failed", "error", err)
		return ""
	}
	defer rows.Close()
	env, n := EnvOther, 0
	for rows.Next() {
		var tags []string
		if err := rows.Scan(&tags); err != nil {
			return ""
		}
		env = stricterOf(env, envOfTags(tags))
		n++
	}
	if rows.Err() != nil || n == 0 {
		return ""
	}
	return env
}

// requestDirect is what the policy says about a request just filed: its kind
// and environment worked out from what it is for.
func (a *API) requestDirect(ctx context.Context, req AccessRequest, dbLabel string) *DirectAllowance {
	switch {
	case req.Principal == "db:restore" || req.Principal == replicaPrincipal:
		return a.actsDirectly(ctx, KindDataRestore, envOfLabel(dbLabel), req.RequesterEmail)
	case strings.HasPrefix(req.Principal, "db:"):
		return a.actsDirectly(ctx, KindDataSession, envOfLabel(dbLabel), req.RequesterEmail)
	}
	return a.actsDirectly(ctx, KindElevatedLogin, a.hostsEnv(ctx, req.AssetHostnames), req.RequesterEmail)
}

// approveByPolicy approves req as the person who asked when the policy lets
// them act directly, and runs what an approval runs. It returns the request
// as it now stands, and whether it was approved.
func (a *API) approveByPolicy(ctx context.Context, req AccessRequest, dbLabel, what string) (AccessRequest, bool) {
	d := a.requestDirect(ctx, req, dbLabel)
	if d == nil {
		return req, false
	}
	out, err := a.store.DecideRequest(ctx, req.ID, "approved", directNote, req.RequesterEmail, true)
	if err != nil {
		a.log.Error("approving a request by the approval policy failed; it waits", "request", req.ID, "error", err)
		return req, false
	}
	a.recordDirect(ctx, d, req.RequesterEmail, joinHosts(req.AssetHostnames), what+" (request "+req.ID+")",
		req.Justification)
	a.closeRequestAlerts(ctx, out)
	switch out.Principal {
	case "db:restore":
		a.restoreApproved(ctx, out.ID, out.RequesterEmail)
	case replicaPrincipal:
		a.replicaDecided(ctx, out.ID, "approved", out.RequesterEmail, directNote)
	}
	return out, true
}

// isNetworkText is an approval item that changes which machines can connect
// (tunnel's network texts all carry a firewall line), not a statement.
func isNetworkText(sql string) bool {
	for _, l := range strings.Split(sql, "\n") {
		if strings.HasPrefix(l, "FIREWALL ") {
			return true
		}
	}
	return false
}

// approveAskByPolicy approves a statement ask (statements, Redis and broker
// changes, network changes) as the person who asked, when the policy lets
// them act directly on db for every kind of item in it. The console then
// runs it under that approval as it would under anyone's, so the agent
// records an approval id and row caps still hold.
func (a *API) approveAskByPolicy(ctx context.Context, req AccessRequest, db Database, items []StatementItem) bool {
	kinds := map[ApprovalKind]bool{}
	for _, it := range items {
		if isNetworkText(it.SQL) {
			kinds[KindDataNetwork] = true
		} else {
			kinds[KindDataChange] = true
		}
	}
	env := envOfLabel(db.Label)
	var d *DirectAllowance
	for k := range kinds {
		if d = a.actsDirectly(ctx, k, env, req.RequesterEmail); d == nil {
			return false
		}
	}
	if d == nil {
		return false
	}
	out, err := a.store.DecideRequest(ctx, req.ID, "approved", directNote, req.RequesterEmail, true)
	if err != nil {
		a.log.Error("approving an ask by the approval policy failed; it waits", "request", req.ID, "error", err)
		return false
	}
	what := "Ran 1 change on " + db.Name
	if len(items) != 1 {
		what = "Ran " + itoa(len(items)) + " changes on " + db.Name
	}
	if len(items) > 0 {
		what += ": " + clip(oneLine(items[0].SQL), 300)
	}
	a.recordDirect(ctx, d, req.RequesterEmail, db.Hostname, what+" (request "+out.ID+")", req.Justification)
	a.closeRequestAlerts(ctx, out)
	return true
}

// directRuns is, per server, the runs the approval policy let through
// without a second admin, recorded once the runs exist.
type directRuns map[string]*DirectAllowance

// runWaits is whether a run asked for by `by` on a server with tags waits
// for a second admin, the server's rule asking for one (needs). When the
// policy lets them act directly it notes so in dr, by hostname.
func (a *API) runWaits(ctx context.Context, needs bool, kind ApprovalKind, tags []string, by, host string, dr directRuns) bool {
	if !needs {
		return false
	}
	if d := a.actsDirectly(ctx, kind, envOfTags(tags), by); d != nil {
		dr[strings.ToLower(host)] = d
		return false
	}
	return true
}

// record records each run dr let through: what, on host.
func (dr directRuns) record(ctx context.Context, a *API, by, host, what string) {
	a.recordDirect(ctx, dr[strings.ToLower(host)], by, host, what, "")
}
