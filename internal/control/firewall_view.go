package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/caduceus"
)

/*
The firewall on each server, read only: the server page's Firewall tab and
Assets › Firewalls. Plan: plans/server-firewall/design.md (PR 1 of 5).

Everything here comes from what the agent last read (agents.firewall, sent
with its facts every ten minutes when something changed, hourly otherwise).
Nothing asks the agent live and nothing changes a firewall.

Readable by whoever sees the whole fleet, like "What runs here": a list of
open ports and who may reach them is the first thing an attacker would draw.
*/

// ServerFirewall is the Firewall tab.
type ServerFirewall struct {
	// Reported is false when the agent has not sent its firewall: it is
	// older than firewall reading, or has not checked in since.
	Reported bool                `json:"reported"`
	Firewall tunnel.HostFirewall `json:"firewall"`
	// Rules are the report's rules with who made each one.
	Rules []ServerFirewallRule `json:"rules"`
	// Listening is each port something listens on, and who the firewall
	// lets reach it.
	Listening []FirewallListen `json:"listening"`
	// ListeningRead is false when the agent has not said what listens (on
	// Windows, so far).
	ListeningRead bool `json:"listeningRead"`
	// Names are servers in Argus by an address a rule names.
	Names map[string]string `json:"names"`
	// Manage is what changing the firewall needs (firewall_routes.go).
	Manage *FirewallManage `json:"manage,omitempty"`
}

// ServerFirewallRule is one rule and who made it.
type ServerFirewallRule struct {
	tunnel.FirewallRule
	// MadeBy is "argus" (a rule Argus added), "database" (Argus added it
	// for a database's "Who can connect") or "outside" (made by hand or by
	// another tool).
	MadeBy string `json:"madeBy"`
	// Database is the database whose "Who can connect" the rule is for.
	Database *FirewallRuleDatabase `json:"database,omitempty"`
}

// FirewallRuleDatabase names the database a rule belongs to.
type FirewallRuleDatabase struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
}

// FirewallListen is one listening port and who can reach it.
type FirewallListen struct {
	Port      int              `json:"port"`
	Process   string           `json:"process,omitempty"`
	Kind      string           `json:"kind,omitempty"`
	LocalOnly bool             `json:"localOnly"`
	Reach     tunnel.PortReach `json:"reach"`
}

type firewallRow struct {
	assetID, hostname, kind, os string
	tags                        []string
	lastSeen                    *time.Time
	firewall, applications      []byte
}

// FirewallFor reads the firewall the agent on asset id last reported.
func (s *Store) FirewallFor(ctx context.Context, id string) (ServerFirewall, error) {
	var row firewallRow
	err := s.db(ctx).QueryRow(ctx, `
		SELECT a.id::text, a.hostname, ag.firewall, ag.applications
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT firewall, applications FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).
		Scan(&row.assetID, &row.hostname, &row.firewall, &row.applications)
	if errors.Is(err, caduceus.ErrNoRows) {
		return ServerFirewall{}, ErrAssetNotFound
	}
	if err != nil {
		return ServerFirewall{}, err
	}
	out := ServerFirewall{Rules: []ServerFirewallRule{}, Listening: []FirewallListen{}, Names: map[string]string{},
		Firewall: tunnel.HostFirewall{Rules: []tunnel.FirewallRule{}}}
	fw, ok := decodeFirewall(row.firewall)
	if !ok {
		return out, nil
	}
	out.Reported, out.Firewall = true, fw

	dbs, err := s.firewallDatabases(ctx, row.assetID)
	if err != nil {
		return out, err
	}
	for _, r := range fw.Rules {
		sr := ServerFirewallRule{FirewallRule: r, MadeBy: "outside"}
		if r.Argus {
			sr.MadeBy = "argus"
			if d, ok := dbs[r.ArgusPort]; ok && r.ArgusPort > 0 {
				sr.MadeBy, sr.Database = "database", &d
			}
		}
		out.Rules = append(out.Rules, sr)
	}

	var inv AppInventory
	if len(row.applications) > 0 && json.Unmarshal(row.applications, &inv) == nil && !inv.CheckedAt.IsZero() {
		out.ListeningRead = true
		out.Listening = firewallListening(fw, inv)
	}

	names, err := s.firewallNames(ctx, fw)
	if err != nil {
		return out, err
	}
	out.Names = names
	return out, nil
}

func decodeFirewall(raw []byte) (tunnel.HostFirewall, bool) {
	var fw tunnel.HostFirewall
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &fw) != nil {
		return fw, false
	}
	if fw.Rules == nil {
		fw.Rules = []tunnel.FirewallRule{}
	}
	return fw, true
}

// firewallListening is each port the agent saw listening, once, with who
// the firewall lets reach it. A port held only on loopback is this server's
// own, whatever the firewall says.
func firewallListening(fw tunnel.HostFirewall, inv AppInventory) []FirewallListen {
	byPort := map[int]*FirewallListen{}
	for _, l := range inv.Listeners {
		if l.Port <= 0 {
			continue
		}
		local := false
		if ip, err := netip.ParseAddr(l.Address); err == nil && ip.IsLoopback() {
			local = true
		}
		f, ok := byPort[l.Port]
		if !ok {
			f = &FirewallListen{Port: l.Port, Process: l.Process, Kind: l.Kind, LocalOnly: true}
			byPort[l.Port] = f
		}
		f.LocalOnly = f.LocalOnly && local
		if f.Process == "" {
			f.Process = l.Process
		}
		if f.Kind == "" {
			f.Kind = l.Kind
		}
	}
	out := make([]FirewallListen, 0, len(byPort))
	for _, f := range byPort {
		if !f.LocalOnly {
			f.Reach = fw.ReachOf(f.Port)
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// firewallDatabases are the databases on an asset, by port.
func (s *Store) firewallDatabases(ctx context.Context, assetID string) (map[int]FirewallRuleDatabase, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, dbname, engine, port FROM databases
		 WHERE asset_id::text = $1 ORDER BY dbname`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]FirewallRuleDatabase{}
	for rows.Next() {
		var d FirewallRuleDatabase
		var port int
		if err := rows.Scan(&d.ID, &d.Name, &d.Engine, &port); err != nil {
			return nil, err
		}
		if _, ok := out[port]; !ok {
			out[port] = d
		}
	}
	return out, rows.Err()
}

// firewallNames maps each single address the rules name to a server in
// Argus that reports it, so a rule reads "from app-01" instead of an
// address. An address two servers report is left as an address.
func (s *Store) firewallNames(ctx context.Context, fw tunnel.HostFirewall) (map[string]string, error) {
	want := map[string]bool{}
	for _, r := range fw.Rules {
		for _, src := range r.Sources {
			if a, err := netip.ParseAddr(src); err == nil && !a.IsLoopback() {
				want[a.String()] = true
			}
		}
	}
	out := map[string]string{}
	if len(want) == 0 {
		return out, nil
	}
	addrs := make([]string, 0, len(want))
	for a := range want {
		addrs = append(addrs, a)
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT addr, min(a.hostname), count(DISTINCT a.id)
		  FROM agents g
		  CROSS JOIN LATERAL unnest(g.addresses) AS addr
		  JOIN assets a ON a.archived_at IS NULL
		   AND (g.hostname = a.hostname OR g.hostname = a.agent_hostname
		        OR g.hostname = split_part(a.hostname, '.', 1))
		 WHERE split_part(addr, '/', 1) = ANY($1)
		 GROUP BY addr`, addrs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var addr, host string
		var n int
		if err := rows.Scan(&addr, &host, &n); err != nil {
			return nil, err
		}
		if n == 1 {
			out[strings.SplitN(addr, "/", 2)[0]] = host
		}
	}
	return out, rows.Err()
}

// FirewallFleetRow is one server on Assets › Firewalls.
type FirewallFleetRow struct {
	AssetID  string   `json:"assetId"`
	Hostname string   `json:"hostname"`
	Kind     string   `json:"kind"`
	OS       string   `json:"os"`
	Tags     []string `json:"tags"`
	// Reported is false when no firewall report has arrived.
	Reported bool `json:"reported"`
	// Firewall is the firewall in charge ("" when none is installed).
	Firewall   string     `json:"firewall"`
	Active     bool       `json:"active"`
	Manageable bool       `json:"manageable"`
	ReadAt     *time.Time `json:"readAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	// Rules counts the rules switched on; Outside those Argus did not add.
	Rules   int `json:"rules"`
	Outside int `json:"outside"`
	// OpenToInternet are listening ports anyone on the internet can reach.
	OpenToInternet []int `json:"openToInternet"`
	// Docker: ports Docker publishes pass around the firewall.
	Docker bool `json:"docker,omitempty"`
}

// FirewallFleet lists every server and workstation with the firewall its
// agent last reported.
func (s *Store) FirewallFleet(ctx context.Context) ([]FirewallFleetRow, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.hostname, a.kind, a.os, a.tags, ag.last_seen_at, ag.firewall, ag.applications
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT last_seen_at, firewall, applications FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL AND a.kind IN ('server', 'workstation')
		 ORDER BY a.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FirewallFleetRow{}
	for rows.Next() {
		var r firewallRow
		if err := rows.Scan(&r.assetID, &r.hostname, &r.kind, &r.os, &r.tags, &r.lastSeen, &r.firewall, &r.applications); err != nil {
			return nil, err
		}
		out = append(out, firewallFleetRowOf(r))
	}
	return out, rows.Err()
}

func firewallFleetRowOf(r firewallRow) FirewallFleetRow {
	row := FirewallFleetRow{AssetID: r.assetID, Hostname: r.hostname, Kind: r.kind, OS: r.os,
		Tags: nonNilStrings(r.tags), OpenToInternet: []int{}}
	fw, ok := decodeFirewall(r.firewall)
	if !ok {
		return row
	}
	row.Reported, row.Firewall, row.Active, row.Manageable = true, fw.Kind, fw.Active, fw.Manageable
	row.Error, row.Docker = fw.Error, fw.Docker
	if !fw.ReadAt.IsZero() {
		at := fw.ReadAt
		row.ReadAt = &at
	}
	for _, rule := range fw.Rules {
		row.Rules++
		if !rule.Argus {
			row.Outside++
		}
	}
	var inv AppInventory
	if fw.Error == "" && len(r.applications) > 0 && json.Unmarshal(r.applications, &inv) == nil {
		for _, l := range firewallListening(fw, inv) {
			if !l.LocalOnly && l.Reach.Public && !slices.Contains(row.OpenToInternet, l.Port) {
				row.OpenToInternet = append(row.OpenToInternet, l.Port)
			}
		}
	}
	return row
}

// getAssetFirewall is the Firewall tab.
func (a *API) getAssetFirewall(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "firewall", err)
		return
	}
	if !seesWholeFleet(sess.Role) {
		// An operator an admin made one of the server's firewall managers.
		_, managers, _, err := a.store.fwServerFor(r.Context(), asset.ID)
		if err != nil || sess.Role != "operator" || !fwCanChange(sess, managers) {
			writeErr(w, http.StatusForbidden, "seeing a server's firewall requires the admin, owner or auditor role")
			return
		}
	}
	out, err := a.store.FirewallFor(r.Context(), asset.ID)
	if err != nil {
		a.failAsset(w, "firewall", err)
		return
	}
	if out.Manage, err = a.fwManage(r.Context(), sess, asset); err != nil {
		a.fail(w, "firewall", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getFirewalls is Assets › Firewalls.
func (a *API) getFirewalls(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing the fleet's firewalls requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.FirewallFleet(r.Context())
	if err != nil {
		a.fail(w, "firewalls", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}
