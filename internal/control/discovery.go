package control

import (
	"context"
	"errors"
	"fmt"
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
	// AssetsUnmonitored are managed hosts where nothing is watching port 22.
	AssetsUnmonitored int `json:"assetsUnmonitored"`
	// UnreviewedHosts are agents reporting from outside the inventory.
	UnreviewedHosts int `json:"unreviewedHosts"`
	IgnoredHosts    int `json:"ignoredHosts"`
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agents (hostname, fqdn, machine_id, os, addresses, ssh_ports, accounts, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (hostname) DO UPDATE SET
			fqdn       = COALESCE(NULLIF(EXCLUDED.fqdn,''), agents.fqdn),
			machine_id = COALESCE(NULLIF(EXCLUDED.machine_id,''), agents.machine_id),
			os         = COALESCE(NULLIF(EXCLUDED.os,''), agents.os),
			addresses  = EXCLUDED.addresses,
			ssh_ports  = EXCLUDED.ssh_ports,
			accounts   = EXCLUDED.accounts,
			updated_at = now()`,
		hostname, f.FQDN, f.MachineID, f.OS,
		nonNilStrings(f.Addresses), nonNilInts(f.SSHPorts), nonNilStrings(f.Accounts))
	return err
}

// DiscoveredHosts lists agents with no matching asset.
//
// state filters by review state; empty returns every unmatched host.
func (s *Store) DiscoveredHosts(ctx context.Context, state string) ([]DiscoveredHost, error) {
	rows, err := s.pool.Query(ctx, `
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
// The asset is created with no principals. Discovery reports which accounts
// exist; it does not get to decide which of them anyone may assume. Turning an
// observation into an entitlement without a person in between would let anyone
// who can start an agent grant themselves a login.
func (s *Store) EnrolHost(ctx context.Context, hostname, actor string) (string, error) {
	tx, err := s.pool.Begin(ctx)
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
	err = tx.QueryRow(ctx, `
		INSERT INTO assets (hostname, address, port, os, agent_hostname,
		                    agent_state, agent_last_seen_at, discovered_by,
		                    host_key_state, principals)
		VALUES ($1,$2,$3,$4,$5,'healthy',now(),$6,'unpinned','{}')
		ON CONFLICT (hostname) DO UPDATE SET
			agent_hostname = EXCLUDED.agent_hostname,
			updated_at     = now()
		RETURNING id::text`,
		assetName, address, port, os, hostname, actor).Scan(&id)
	if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE agents SET enrollment_state = 'enrolled', reviewed_by = $2,
		                  reviewed_at = now(), matched_asset = true, updated_at = now()
		WHERE hostname = $1`, hostname, actor); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// IgnoreHost records a deliberate decision not to manage a host.
//
// Kept rather than deleted, and with a note: a dismissal that leaves no trace
// is indistinguishable from a host nobody ever looked at, and the next reviewer
// has to redo the work.
func (s *Store) IgnoreHost(ctx context.Context, hostname, actor, note string) error {
	tag, err := s.pool.Exec(ctx, `
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
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM assets),
		  (SELECT count(*) FROM assets WHERE agent_state = 'healthy'),
		  (SELECT count(*) FROM assets WHERE agent_state = 'stale'),
		  (SELECT count(*) FROM assets WHERE agent_state <> 'healthy'),
		  (SELECT count(*) FROM agents WHERE matched_asset = false AND enrollment_state = 'unreviewed'),
		  (SELECT count(*) FROM agents WHERE matched_asset = false AND enrollment_state = 'ignored')`).
		Scan(&c.Assets, &c.AssetsWithAgent, &c.AssetsAgentStale,
			&c.AssetsUnmonitored, &c.UnreviewedHosts, &c.IgnoredHosts)
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
