package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud › Risks & waste: security risks, what is paid for and not in use,
and firewall rules nothing needs, across every connected account.

	GET    /api/v1/cloud/findings           everything, with what is kept
	POST   /api/v1/cloud/findings/keep      keep one finding, with a reason
	DELETE /api/v1/cloud/findings/keep      stop keeping it

Findings are worked out on each request from the stored inventory (the
5-minute sync) and what Argus's agents report; nothing here calls a cloud.
They are suggestions: a fix is the ordinary change, asked for from the
console with its own preview, approval and audit entry. See cloud.Findings
for what each check means and how it says how it knows.
*/

// cloudFindingKinds are the kinds the checks read.
var cloudFindingKinds = []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindFirewall, cloud.KindAddress, cloud.KindSnapshot,
	cloud.KindImage, cloud.KindLoadBalancer, cloud.KindTargetGroup, cloud.KindAccount, cloud.KindUser, cloud.KindBucket, cloud.KindDatabase,
	cloud.KindReservedInstance, cloud.KindSavingsPlan, cloud.KindCapacityReservation}

// cloudFindingItem is a finding with the account it is in, and whether a
// person kept it.
type cloudFindingItem struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Environment string `json:"environment"`
	cloud.Finding
	Kept *CloudFindingKeep `json:"kept,omitempty"`
}

// CloudFindingKeep is a finding someone chose to keep.
type CloudFindingKeep struct {
	AccountID   string     `json:"accountId"`
	FindingID   string     `json:"findingId"`
	Basis       string     `json:"-"`
	Reason      string     `json:"reason"`
	KeptBy      string     `json:"keptBy"`
	KeptAt      time.Time  `json:"keptAt"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	UntilChange bool       `json:"untilChange"`
}

// cloudFindingsAccount says which checks could be made for one account.
type cloudFindingsAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// AccountChecks is whether people, keys, buckets and databases have
	// been read; without them those checks were not made.
	AccountChecks bool `json:"accountChecks"`
}

func (a *API) getCloudFindings(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	accounts, err := a.store.CloudAccounts(ctx)
	if err != nil {
		a.fail(w, "cloud findings", err)
		return
	}
	byAccount := map[string][]cloud.Resource{}
	var instances []CloudItem
	for _, k := range cloudFindingKinds {
		items, err := a.store.CloudResources(ctx, k)
		if err != nil {
			a.fail(w, "cloud findings", err)
			return
		}
		if k == cloud.KindInstance {
			instances = items
		}
		for _, it := range items {
			byAccount[it.AccountID] = append(byAccount[it.AccountID], it.Resource)
		}
	}
	facts, err := a.cloudServerFacts(ctx, instances, now)
	if err != nil {
		a.fail(w, "cloud findings", err)
		return
	}
	keeps, err := a.store.CloudFindingKeeps(ctx)
	if err != nil {
		a.fail(w, "cloud findings", err)
		return
	}

	out := []cloudFindingItem{}
	kept := []cloudFindingItem{}
	seen := []cloudFindingsAccount{}
	var monthly float64
	for _, c := range accounts {
		rs := byAccount[c.ID]
		acc := cloudFindingsAccount{ID: c.ID, Name: c.Name}
		for _, x := range rs {
			if x.Kind == cloud.KindAccount {
				acc.AccountChecks = true
			}
		}
		seen = append(seen, acc)
		for _, f := range cloud.FindingsIn(c.Provider, rs, facts, now) {
			it := cloudFindingItem{AccountID: c.ID, AccountName: c.Name, Environment: c.Environment, Finding: f}
			if k, ok := keeps[c.ID+"/"+f.ID]; ok && k.holds(f.Basis, now) {
				it.Kept = &k
				kept = append(kept, it)
				continue
			}
			if f.Group == cloud.GroupWaste {
				monthly += f.Monthly
			}
			out = append(out, it)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "kept": kept, "accounts": seen, "monthly": math.Round(monthly*100) / 100, "checkedAt": now,
	})
}

// holds reports whether a keep still hides its finding.
func (k CloudFindingKeep) holds(basis string, now time.Time) bool {
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return false
	}
	if k.UntilChange && k.Basis != basis {
		return false
	}
	return true
}

// cloudServerFacts is what agents say about the instances matched to
// servers, keyed by instance id.
func (a *API) cloudServerFacts(ctx context.Context, instances []CloudItem, now time.Time) (map[string]cloud.ServerFacts, error) {
	if len(instances) == 0 {
		return map[string]cloud.ServerFacts{}, nil
	}
	assets, err := a.store.Assets(ctx)
	if err != nil {
		return nil, err
	}
	reported, err := a.store.AgentAddresses(ctx)
	if err != nil {
		return nil, err
	}
	agentInstances, err := a.store.AgentInstances(ctx)
	if err != nil {
		return nil, err
	}
	matchServers(instances, assets, reported, agentInstances)
	var ids []string
	for _, it := range instances {
		if it.Server != nil {
			ids = append(ids, it.Server.ID)
		}
	}
	byServer, err := a.store.ServerFactsFor(ctx, ids, now)
	if err != nil {
		return nil, err
	}
	out := map[string]cloud.ServerFacts{}
	for _, it := range instances {
		if it.Server == nil {
			continue
		}
		f := byServer[it.Server.ID]
		f.ServerID, f.Hostname = it.Server.ID, it.Server.Hostname
		out[it.Resource.ID] = f
	}
	return out, nil
}

// ServerFactsFor reads, for each server, its agent's last check-in, 30 days
// of usage, the last session through Argus and what it listens on.
func (s *Store) ServerFactsFor(ctx context.Context, ids []string, now time.Time) (map[string]cloud.ServerFacts, error) {
	out := map[string]cloud.ServerFacts{}
	if len(ids) == 0 {
		return out, nil
	}
	since := now.Add(-historyRetention)
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, ag.hostname, ag.last_seen_at, ag.applications,
		       u.first, u.cpu95, u.net,
		       (SELECT max(started_at) FROM sessions se WHERE se.asset_id = a.id)
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT hostname, last_seen_at, applications FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		  LEFT JOIN LATERAL (
		       SELECT min(bucket) AS first,
		              percentile_cont(0.95) WITHIN GROUP (ORDER BY cpu_sum / cpu_n) FILTER (WHERE cpu_n > 0) AS cpu95,
		              sum(rx_sum + tx_sum) / NULLIF(sum(net_n), 0) AS net
		         FROM usage_history h
		        WHERE h.hostname = ag.hostname AND h.bucket >= $2) u ON TRUE
		 WHERE a.id::text = ANY($1) AND a.archived_at IS NULL`, ids, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id         string
			host       *string
			seen       *time.Time
			apps       []byte
			first      *time.Time
			cpu95, net *float64
			last       *time.Time
		)
		if err := rows.Scan(&id, &host, &seen, &apps, &first, &cpu95, &net, &last); err != nil {
			return nil, err
		}
		f := cloud.ServerFacts{SeenAt: seen, CPUP95: cpu95, NetBps: net, LastSession: last}
		if host != nil {
			f.Hostname = *host
		}
		if first != nil {
			f.HistoryFor = now.Sub(*first)
		}
		var inv AppInventory
		if len(apps) > 0 && string(apps) != "null" && json.Unmarshal(apps, &inv) == nil && !inv.CheckedAt.IsZero() {
			at := inv.CheckedAt
			f.ListenAt = &at
			f.Listening = reachablePorts(inv)
		}
		out[id] = f
	}
	return out, rows.Err()
}

// reachablePorts is every TCP port something listens on that another
// machine could reach, from an agent's application report.
func reachablePorts(inv AppInventory) []int {
	var ports []int
	for _, l := range inv.Listeners {
		if listenOf(AppAddress{Address: l.Address, Port: l.Port}).LocalOnly {
			continue
		}
		if !slices.Contains(ports, l.Port) {
			ports = append(ports, l.Port)
		}
	}
	slices.Sort(ports)
	return ports
}

// CloudFindingKeeps reads every keep, keyed "account/finding".
func (s *Store) CloudFindingKeeps(ctx context.Context) (map[string]CloudFindingKeep, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT account_id::text, finding_id, basis, reason, kept_by, kept_at, expires_at, until_change
		FROM cloud_finding_keeps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CloudFindingKeep{}
	for rows.Next() {
		var k CloudFindingKeep
		if err := rows.Scan(&k.AccountID, &k.FindingID, &k.Basis, &k.Reason, &k.KeptBy, &k.KeptAt, &k.ExpiresAt, &k.UntilChange); err != nil {
			return nil, err
		}
		out[k.AccountID+"/"+k.FindingID] = k
	}
	return out, rows.Err()
}

// SaveCloudFindingKeep keeps a finding, replacing an earlier keep of it.
func (s *Store) SaveCloudFindingKeep(ctx context.Context, k CloudFindingKeep) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO cloud_finding_keeps (account_id, finding_id, basis, reason, kept_by, kept_at, expires_at, until_change)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (account_id, finding_id) DO UPDATE SET basis = EXCLUDED.basis, reason = EXCLUDED.reason, kept_by = EXCLUDED.kept_by,
			kept_at = EXCLUDED.kept_at, expires_at = EXCLUDED.expires_at, until_change = EXCLUDED.until_change`,
		k.AccountID, k.FindingID, k.Basis, k.Reason, k.KeptBy, k.KeptAt, k.ExpiresAt, k.UntilChange)
	return err
}

// ErrCloudFindingKeepNotFound is a keep that is not there.
var ErrCloudFindingKeepNotFound = errors.New("that finding is not kept")

// DeleteCloudFindingKeep stops keeping a finding.
func (s *Store) DeleteCloudFindingKeep(ctx context.Context, accountID, findingID string) (CloudFindingKeep, error) {
	var k CloudFindingKeep
	err := s.db(ctx).QueryRow(ctx, `DELETE FROM cloud_finding_keeps WHERE account_id::text = $1 AND finding_id = $2
		RETURNING account_id::text, finding_id, reason, kept_by, kept_at`, accountID, findingID).
		Scan(&k.AccountID, &k.FindingID, &k.Reason, &k.KeptBy, &k.KeptAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return k, ErrCloudFindingKeepNotFound
		}
		return k, err
	}
	return k, nil
}

// MaxKeepHigh is the longest a High risk may be kept.
const MaxKeepHigh = 90 * 24 * time.Hour

type cloudKeepIn struct {
	AccountID string `json:"accountId"`
	FindingID string `json:"findingId"`
	Reason    string `json:"reason"`
	// Days is 7, 30 or 90; 0 with UntilChange keeps it until it changes.
	Days        int  `json:"days"`
	UntilChange bool `json:"untilChange"`
}

func (a *API) postCloudFindingKeep(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	var in cloudKeepIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len([]rune(in.Reason)) > 300 {
		writeErr(w, http.StatusBadRequest, "say why you are keeping it, in at most 300 characters")
		return
	}
	if in.Days != 0 && in.Days != 7 && in.Days != 30 && in.Days != 90 {
		writeErr(w, http.StatusBadRequest, "keep it for 7, 30 or 90 days, or until it changes")
		return
	}
	if in.Days == 0 && !in.UntilChange {
		writeErr(w, http.StatusBadRequest, "keep it for 7, 30 or 90 days, or until it changes")
		return
	}
	c, err := a.store.CloudAccount(r.Context(), in.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pick a connected account")
		return
	}
	// The finding must be one there is now, so its basis and severity are
	// the server's, not the caller's.
	f, ok, err := a.cloudFindingNow(r.Context(), c, in.FindingID)
	if err != nil {
		a.fail(w, "cloud findings", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "that finding is no longer there; it may have been fixed")
		return
	}
	now := time.Now().UTC()
	k := CloudFindingKeep{AccountID: c.ID, FindingID: f.ID, Basis: f.Basis, Reason: in.Reason, KeptBy: actor, KeptAt: now, UntilChange: in.UntilChange}
	if in.Days > 0 {
		at := now.Add(time.Duration(in.Days) * 24 * time.Hour)
		k.ExpiresAt = &at
	}
	if f.Severity == cloud.SeverityHigh && (k.ExpiresAt == nil || k.ExpiresAt.Sub(now) > MaxKeepHigh) {
		writeErr(w, http.StatusBadRequest, "a High risk can be kept for at most 90 days, so it is looked at again")
		return
	}
	if err := a.store.SaveCloudFindingKeep(r.Context(), k); err != nil {
		a.fail(w, "cloud findings", err)
		return
	}
	how := "until it changes"
	if k.ExpiresAt != nil {
		how = "until " + k.ExpiresAt.Format(time.DateOnly)
		if k.UntilChange {
			how += " or it changes"
		}
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.finding.kept", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: fmt.Sprintf("kept %s risk %q on %s %s, %s: %s", f.Severity, f.Title, f.Kind, f.Name, how, k.Reason)})
	writeJSON(w, http.StatusOK, k)
}

func (a *API) deleteCloudFindingKeep(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	acct, id := r.URL.Query().Get("accountId"), r.URL.Query().Get("findingId")
	c, err := a.store.CloudAccount(r.Context(), acct)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pick a connected account")
		return
	}
	k, err := a.store.DeleteCloudFindingKeep(r.Context(), c.ID, id)
	if errors.Is(err, ErrCloudFindingKeepNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud findings", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.finding.unkept", Severity: "info", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: fmt.Sprintf("stopped keeping %s (kept by %s: %s)", k.FindingID, k.KeptBy, k.Reason)})
	w.WriteHeader(http.StatusNoContent)
}

// cloudFindingNow finds one finding in one account as it is now.
func (a *API) cloudFindingNow(ctx context.Context, c CloudAccount, id string) (cloud.Finding, bool, error) {
	var rs []cloud.Resource
	var instances []CloudItem
	for _, k := range cloudFindingKinds {
		items, err := a.store.CloudResources(ctx, k)
		if err != nil {
			return cloud.Finding{}, false, err
		}
		for _, it := range items {
			if it.AccountID != c.ID {
				continue
			}
			rs = append(rs, it.Resource)
			if k == cloud.KindInstance {
				instances = append(instances, it)
			}
		}
	}
	now := time.Now().UTC()
	facts, err := a.cloudServerFacts(ctx, instances, now)
	if err != nil {
		return cloud.Finding{}, false, err
	}
	for _, f := range cloud.FindingsIn(c.Provider, rs, facts, now) {
		if f.ID == id {
			return f, true, nil
		}
	}
	return cloud.Finding{}, false, nil
}
