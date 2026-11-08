package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HostFacts is what an agent reports about the machine it runs on.
//
// Mirrors agent.Facts. Nothing here is a secret — account names, addresses and
// listening ports are visible to any local user — because the purpose is
// coverage, not reconnaissance.
type HostFacts struct {
	FQDN      string   `json:"fqdn,omitempty"`
	MachineID string   `json:"machine_id,omitempty"`
	OS        string   `json:"os,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	SSHPorts  []int    `json:"ssh_ports,omitempty"`
	Accounts  []string `json:"accounts,omitempty"`
	// Applications is what runs on the host. Nil from an agent older than
	// application discovery, which leaves what the row holds.
	Applications *AppInventory `json:"applications,omitempty"`
	// System is what kind of machine the host is. Nil from an agent older
	// than system details, which leaves what the row holds.
	System *HostSystem `json:"system,omitempty"`
	// AndroidApps is what is installed on a device. Nil from a server or an
	// app older than app reporting, which leaves what the row holds.
	AndroidApps *AndroidApps `json:"android_apps,omitempty"`
}

// DiscoveredHost is an agent reporting from somewhere the inventory has no
// entry for.
type DiscoveredHost struct {
	Hostname    string    `json:"hostname"`
	FQDN        string    `json:"fqdn,omitempty"`
	MachineID   string    `json:"machineId,omitempty"`
	OS          string    `json:"os,omitempty"`
	Addresses   []string  `json:"addresses"`
	SSHPorts    []int     `json:"sshPorts"`
	Accounts    []string  `json:"accounts"`
	Version     string    `json:"version"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`

	State      string     `json:"state"`
	ReviewNote string     `json:"reviewNote,omitempty"`
	ReviewedBy string     `json:"reviewedBy,omitempty"`
	ReviewedAt *time.Time `json:"reviewedAt,omitempty"`
}

// Coverage is the two-sided answer to "does Argus see everything?".
//
// Both directions are gaps and neither implies the other. An asset with no
// agent is a host where a direct connection to port 22 leaves no trace. An
// agent with no asset is a privileged host Argus is not managing at all. A
// dashboard that reported only the first would look complete while missing
// entire machines.
type Coverage struct {
	Assets           int `json:"assets"`
	AssetsWithAgent  int `json:"assetsWithAgent"`
	AssetsAgentStale int `json:"assetsAgentStale"`
	// AssetsUnmonitored are hosts where a direct connection leaves no trace and
	// an agent could close the gap.
	//
	// Counts SSH assets only. A Windows host has no agent available yet, so
	// listing it here would be a gap nobody can act on — and reporting those
	// trains people to ignore the ones they can.
	AssetsUnmonitored int `json:"assetsUnmonitored"`
	// UnreviewedHosts are agents reporting from outside the inventory.
	UnreviewedHosts int `json:"unreviewedHosts"`
	IgnoredHosts    int `json:"ignoredHosts"`

	// SSHAssets and RDPAssets break the fleet down by how it is reached.
	SSHAssets int `json:"sshAssets"`
	RDPAssets int `json:"rdpAssets"`
	// RDPAwaitingAgent is the number of Remote Desktop assets that cannot be
	// covered yet because no Windows agent exists. Stated rather than hidden:
	// it is a known limit of the product, not a deployment mistake, and an
	// operator reading a coverage figure deserves to know which it is.
	RDPAwaitingAgent int `json:"rdpAwaitingAgent"`
}

// RecordFacts stores what an agent reports about its host.
//
// Separate from Heartbeat because facts change on a different timescale from
// liveness: an agent heartbeats every thirty seconds and its OS changes on a
// reboot. Folding them together would rewrite six columns a minute for no
// reason.
func (s *Store) RecordFacts(ctx context.Context, hostname string, f HostFacts) error {
	if hostname == "" {
		return errors.New("hostname is required")
	}
	var apps []byte // nil is SQL NULL: keep what the row holds
	if inv := f.Applications.bounded(); inv != nil {
		b, err := json.Marshal(inv)
		if err != nil {
			return err
		}
		apps = b
	}
	var system []byte // nil keeps what the row holds, as for apps
	if sys := f.System.bounded(); sys != nil {
		b, err := json.Marshal(sys)
		if err != nil {
			return err
		}
		system = b
	}
	var androidApps []byte // nil keeps what the row holds, as for apps
	if aa := f.AndroidApps.bounded(); aa != nil {
		b, err := json.Marshal(aa)
		if err != nil {
			return err
		}
		androidApps = b
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO agents (hostname, fqdn, machine_id, os, addresses, ssh_ports, accounts, applications, system, android_apps, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10::jsonb,now())
		ON CONFLICT (hostname) DO UPDATE SET
			fqdn       = COALESCE(NULLIF(EXCLUDED.fqdn,''), agents.fqdn),
			machine_id = COALESCE(NULLIF(EXCLUDED.machine_id,''), agents.machine_id),
			os         = COALESCE(NULLIF(EXCLUDED.os,''), agents.os),
			addresses  = EXCLUDED.addresses,
			ssh_ports  = EXCLUDED.ssh_ports,
			accounts   = EXCLUDED.accounts,
			applications = COALESCE(EXCLUDED.applications, agents.applications),
			system     = COALESCE(EXCLUDED.system, agents.system),
			android_apps = COALESCE(EXCLUDED.android_apps, agents.android_apps),
			updated_at = now()`,
		hostname, f.FQDN, f.MachineID, f.OS,
		nonNilStrings(f.Addresses), nonNilInts(f.SSHPorts), nonNilStrings(f.Accounts), apps, system, androidApps)
	return err
}

// DiscoveredHosts lists agents with no matching asset.
//
// state filters by review state; empty returns every unmatched host.
func (s *Store) DiscoveredHosts(ctx context.Context, state string) ([]DiscoveredHost, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT hostname, COALESCE(fqdn,''), COALESCE(machine_id,''), os,
		       addresses, ssh_ports, accounts, version,
		       first_seen_at, last_seen_at,
		       enrollment_state, review_note, COALESCE(reviewed_by,''), reviewed_at
		FROM agents
		WHERE matched_asset = false
		  AND ($1 = '' OR enrollment_state = $1)
		ORDER BY last_seen_at DESC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DiscoveredHost{}
	for rows.Next() {
		var h DiscoveredHost
		if err := rows.Scan(&h.Hostname, &h.FQDN, &h.MachineID, &h.OS,
			&h.Addresses, &h.SSHPorts, &h.Accounts, &h.Version,
			&h.FirstSeenAt, &h.LastSeenAt,
			&h.State, &h.ReviewNote, &h.ReviewedBy, &h.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ErrAlreadyReviewed reports an enrolment or dismissal that has already
// happened, so a second attempt is not presented as a fresh decision.
var ErrAlreadyReviewed = errors.New("host has already been reviewed")

// EnrolHost promotes a discovered host into a managed asset.
//
// Discovery reports which accounts exist; it does not get to decide which of
// them anyone may assume. Turning an observation into an entitlement without a
// person in between would let anyone who can start an agent grant themselves a
// login. So the principals and the people come from the approving admin, never
// from the agent's report, and with neither the asset is created granting
// nothing.
//
// Asking for them here rather than afterwards is the point. An approval that
// granted nothing left the admin who had just added a server unable to reach
// it, and the way on was an Edit dialog and a second dialog for assignments
// that nothing pointed at. The asset, its accounts and who may use them are
// written in one transaction, so a half-approved server never exists.
//
// personal gives each member of the teams an account of their own on the
// server, so a team can be given a server without naming any shared account.
func (s *Store) EnrolHost(
	ctx context.Context, hostname, actor string, principals, people, teams []string,
	personal bool,
) (string, error) {
	return s.EnrolHostAs(ctx, "server", hostname, actor, principals, people, teams, personal)
}

// EnrolHostAs is EnrolHost with the asset's kind: "server" or
// "workstation". The two are stored and opened the same way; the kind says
// whether a person sits at it, which the console shows and later decides
// who is asked before a session opens. A device is EnrolDevice.
func (s *Store) EnrolHostAs(
	ctx context.Context, kind, hostname, actor string, principals, people, teams []string,
	personal bool,
) (string, error) {
	switch kind {
	case "":
		kind = "server"
	case "server", "workstation":
	default:
		return "", ErrInvalidAsset{Reason: "kind must be server or workstation; a device is enrolled as android"}
	}
	return s.enrol(ctx, hostname, actor, principals, people, teams, personal, kind, nil)
}

// DeviceEnrol is what an Android device is enrolled with beyond a server:
// how it is controlled. Its accounts are fixed (view and control), so the
// admin picks which of those people get, not what exists.
type DeviceEnrol struct {
	// ControlMode is "attended" (the person holding it accepts each
	// session) or "unattended".
	ControlMode string
	// ControlNeedsApproval makes "control" an elevated principal on it.
	ControlNeedsApproval bool
}

// EnrolDevice is EnrolHost for an Android device: the asset is stored the
// way a device opens (protocol rdp, desktop mode screen, kind android) with
// the control mode the admin chose, and only view and control may be given.
func (s *Store) EnrolDevice(
	ctx context.Context, hostname, actor string, principals, people, teams []string,
	dev DeviceEnrol,
) (string, error) {
	switch dev.ControlMode {
	case "":
		dev.ControlMode = "attended"
	case "attended", "unattended":
	default:
		return "", ErrInvalidAsset{Reason: "control mode must be attended or unattended"}
	}
	for _, p := range dedupe(principals) {
		if p != DeviceView && p != DeviceControl {
			return "", ErrInvalidAsset{Reason: "a device has no accounts: give view, control or both"}
		}
	}
	return s.enrol(ctx, hostname, actor, principals, people, teams, false, "android", &dev)
}

func (s *Store) enrol(
	ctx context.Context, hostname, actor string, principals, people, teams []string,
	personal bool, kind string, dev *DeviceEnrol,
) (string, error) {
	principals = dedupe(principals)
	if len(principals) > maxPrincipals {
		return "", ErrInvalidAsset{Reason: fmt.Sprintf(
			"a server may list at most %d login accounts", maxPrincipals)}
	}
	for _, p := range principals {
		if !rePrincipal.MatchString(p) {
			return "", ErrInvalidAsset{Reason: fmt.Sprintf(
				"%q is not an account name: letters, digits, dot, underscore and hyphen", p)}
		}
	}
	emails := make([]string, 0, len(people))
	for _, e := range people {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			emails = append(emails, e)
		}
	}
	emails = dedupe(emails)
	teams = dedupe(teams)
	if len(teams) == 0 {
		personal = false
	}
	if (len(emails) > 0 || (len(teams) > 0 && !personal)) && len(principals) == 0 {
		// "May connect, as nobody" is a state with no meaning; SetAssignment
		// refuses it for the same reason.
		return "", ErrInvalidAsset{Reason: "pick at least one login account for the people who may connect"}
	}

	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		fqdn, os  string
		addresses []string
		ports     []int
		state     string
	)
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(fqdn,''), os, addresses, ssh_ports, enrollment_state
		FROM agents WHERE hostname = $1 FOR UPDATE`, hostname).
		Scan(&fqdn, &os, &addresses, &ports, &state)
	if err != nil {
		return "", err
	}
	if state != "unreviewed" {
		return "", fmt.Errorf("%w: %s is %s", ErrAlreadyReviewed, hostname, state)
	}

	// Prefer the fully qualified name: it is what the inventory would have used
	// and what a certificate would name.
	assetName := fqdn
	if assetName == "" {
		assetName = hostname
	}
	address := ""
	if len(addresses) > 0 {
		address = addresses[0]
	}
	port := 22
	if len(ports) > 0 {
		port = ports[0]
	}

	var id string
	// Owned by the console from here on. Enrolling is an administrator deciding
	// this host is ours to manage, so a gateway publishing a file entry for the
	// same name must not quietly write over what they do with it next.
	if dev == nil {
		err = tx.QueryRow(ctx, `
		INSERT INTO assets (hostname, address, port, os, agent_hostname,
		                    agent_state, agent_last_seen_at, discovered_by,
		                    host_key_state, principals, source, kind)
		VALUES ($1,$2,$3,$4,$5,'healthy',now(),$6,'unpinned',$7,'console',$8)
		ON CONFLICT (hostname) DO UPDATE SET
			agent_hostname = EXCLUDED.agent_hostname,
			source         = 'console',
			-- A device row keeps its kind: what makes it a device is how it
			-- is stored, which this path does not write.
			kind           = CASE WHEN assets.kind = 'android' THEN assets.kind ELSE EXCLUDED.kind END,
			principals     = ARRAY(SELECT DISTINCT unnest(assets.principals || EXCLUDED.principals)),
			updated_at     = now()
		RETURNING id::text`,
			assetName, address, port, os, hostname, actor, principals, kind).Scan(&id)
	} else {
		// A device: stored the way it opens (see AssetInput.Kind), with the
		// control mode chosen now. A row of the same name that was a server
		// becomes the device, the agent of that name being the device now.
		err = tx.QueryRow(ctx, `
		INSERT INTO assets (hostname, address, port, os, agent_hostname,
		                    agent_state, agent_last_seen_at, discovered_by,
		                    host_key_state, principals, source,
		                    protocol, desktop_mode, credential_mode, domain,
		                    kind, control_mode, control_needs_approval)
		VALUES ($1,$2,$3,$4,$5,'healthy',now(),$6,'unpinned',$7,'console',
		        'rdp','screen','injected-password','','android',$8,$9)
		ON CONFLICT (hostname) DO UPDATE SET
			agent_hostname = EXCLUDED.agent_hostname,
			source         = 'console',
			principals     = EXCLUDED.principals,
			protocol       = 'rdp',
			desktop_mode   = 'screen',
			credential_mode = 'injected-password',
			domain         = '',
			kind           = 'android',
			control_mode   = EXCLUDED.control_mode,
			control_needs_approval = EXCLUDED.control_needs_approval,
			updated_at     = now()
		RETURNING id::text`,
			assetName, address, port, os, hostname, actor, principals,
			dev.ControlMode, dev.ControlNeedsApproval).Scan(&id)
	}
	if err != nil {
		return "", err
	}

	for _, e := range emails {
		if _, err := tx.Exec(ctx, `
			INSERT INTO asset_assignments (asset_id, user_email, principals, granted_by)
			VALUES ($1::uuid, $2, $3, $4)
			ON CONFLICT (asset_id, user_email) DO UPDATE SET
				principals = EXCLUDED.principals,
				granted_by = EXCLUDED.granted_by,
				granted_at = now()`,
			id, e, principals, strings.ToLower(actor)); err != nil {
			return "", err
		}
	}

	if err := insertTeamAssignments(ctx, tx, id, teams, principals, personal, actor); err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE agents SET enrollment_state = 'enrolled', reviewed_by = $2,
		                  reviewed_at = now(), matched_asset = true, updated_at = now()
		WHERE hostname = $1`, hostname, actor); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if personal {
		if err := s.ensurePersonalLoginNames(ctx); err != nil {
			return id, err
		}
	}
	return id, nil
}

// IgnoreHost records a deliberate decision not to manage a host.
//
// Kept rather than deleted, and with a note: a dismissal that leaves no trace
// is indistinguishable from a host nobody ever looked at, and the next reviewer
// has to redo the work.
func (s *Store) IgnoreHost(ctx context.Context, hostname, actor, note string) error {
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE agents SET enrollment_state = 'ignored', review_note = $3,
		                  reviewed_by = $2, reviewed_at = now(), updated_at = now()
		WHERE hostname = $1 AND enrollment_state = 'unreviewed'`,
		hostname, actor, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyReviewed
	}
	return nil
}

// Coverage counts both kinds of gap.
func (s *Store) Coverage(ctx context.Context) (Coverage, error) {
	var c Coverage
	err := s.db(ctx).QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM assets),
		  (SELECT count(*) FROM assets WHERE agent_state = 'healthy'),
		  (SELECT count(*) FROM assets WHERE agent_state = 'stale'),
		  (SELECT count(*) FROM assets WHERE agent_state <> 'healthy' AND protocol = 'ssh'),
		  (SELECT count(*) FROM agents WHERE matched_asset = false AND enrollment_state = 'unreviewed'),
		  (SELECT count(*) FROM agents WHERE matched_asset = false AND enrollment_state = 'ignored'),
		  (SELECT count(*) FROM assets WHERE protocol = 'ssh'),
		  (SELECT count(*) FROM assets WHERE protocol = 'rdp'),
		  (SELECT count(*) FROM assets WHERE protocol = 'rdp' AND agent_state <> 'healthy')`).
		Scan(&c.Assets, &c.AssetsWithAgent, &c.AssetsAgentStale,
			&c.AssetsUnmonitored, &c.UnreviewedHosts, &c.IgnoredHosts,
			&c.SSHAssets, &c.RDPAssets, &c.RDPAwaitingAgent)
	return c, err
}

// Postgres array parameters reject nil, and a host with no addresses is a
// legitimate answer rather than a missing one.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNilInts(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}
