package tunnel

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

/*
A streaming replica of a Postgres server Argus manages: a second server that
copies the primary byte for byte and takes reads. On the primary, Argus makes
one role that may only replicate, and one physical slot so the primary keeps
the WAL the replica still needs. Argus checks the primary's settings; the
one edit it makes is a single pg_hba.conf line for the replica, followed by a
reload. A setting that needs a restart is shown with the exact value to set,
for whoever runs the server: Argus never restarts a primary.
*/

// PGReplica is what the replica operations on the primary act on.
type PGReplica struct {
	// Name names the replica: the role is argus_repl_<name> and the slot
	// argus_replica_<name>.
	Name string `json:"name"`
	// Address is the replica's IP address as the primary sees it.
	Address string `json:"address,omitempty"`
	// Verifier is the SCRAM-SHA-256 verifier of the replication password,
	// for replica-prepare. The password goes from the replica's agent to the
	// replica's own Postgres; the primary only ever sees the verifier.
	Verifier string `json:"verifier,omitempty"`
	// By names who asked, for the comment on the role.
	By string `json:"by,omitempty"`
}

var replicaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Check says what is wrong with r, or nil.
func (r PGReplica) Check() error {
	if !replicaName.MatchString(r.Name) {
		return errors.New("name the replica with a lowercase letter, then lowercase letters, digits and underscores, up to 40")
	}
	if r.Address != "" {
		if _, err := netip.ParseAddr(r.Address); err != nil {
			return errors.New("the replica's address is not an IP address: " + r.Address)
		}
	}
	return nil
}

// Role is the replication role for r.
func (r PGReplica) Role() string { return "argus_repl_" + r.Name }

// Slot is the physical replication slot for r.
func (r PGReplica) Slot() string { return "argus_replica_" + r.Name }

// PGReplicaCheck is what replica-check found on the primary.
type PGReplicaCheck struct {
	Version    string `json:"version"`
	VersionNum int    `json:"versionNum"`
	WalLevel   string `json:"walLevel"`
	// Senders and Slots: how many the server allows and how many are in use.
	MaxSenders   int    `json:"maxSenders"`
	SendersInUse int    `json:"sendersInUse"`
	MaxSlots     int    `json:"maxSlots"`
	SlotsInUse   int    `json:"slotsInUse"`
	Listen       string `json:"listen"`
	SSL          bool   `json:"ssl"`
	// HBA is the pg_hba.conf line that lets the replica in, if one does:
	// "line 98: hostssl replication argus_repl_r1 10.0.2.15/32 scram-sha-256".
	HBA   string `json:"hba,omitempty"`
	HBAOK bool   `json:"hbaOk,omitempty"`
	// HBALine is the line to add when none does.
	HBALine string `json:"hbaLine"`
	// Role and Slot say whether they exist already, from an earlier try.
	Role       string `json:"role"`
	RoleExists bool   `json:"roleExists"`
	Slot       string `json:"slot"`
	SlotExists bool   `json:"slotExists"`
	SlotActive bool   `json:"slotActive"`
	// Missing says, in words, what must change before a replica can stream;
	// Restart is whether a change in it needs the server restarted, rather
	// than reloaded.
	Missing []string `json:"missing,omitempty"`
	Restart bool     `json:"restart,omitempty"`
}

// Ready is whether nothing is missing.
func (c PGReplicaCheck) Ready() bool { return len(c.Missing) == 0 }

// Replica operations on a primary, for a replica-primary job.
const (
	ReplicaCheck   = "check"
	ReplicaPrepare = "prepare"
	ReplicaAllow   = "allow"
	ReplicaDrop    = "drop"
)

// ReplicaStageVerifier is the progress stage in which a clone job hands the
// control plane the verifier of the replication password it made, as the
// progress detail. The password itself stays on the replica: the control
// plane has the primary make the role with the verifier, then sends go.
const ReplicaStageVerifier = "verifier"

// GrantReplica is what a replica job acts on.
type GrantReplica struct {
	PGReplica
	// Op is the operation, for replica-primary.
	Op string `json:"op,omitempty"`
	// The primary, for replica-clone: where to copy from, and its major
	// version, which this server's Postgres must match.
	PrimaryHost    string `json:"primaryHost,omitempty"`
	PrimaryPort    int    `json:"primaryPort,omitempty"`
	PrimaryVersion int    `json:"primaryVersion,omitempty"`
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)

// Validate says what is wrong with r for job, or nil.
func (r *GrantReplica) Validate(job string) error {
	if r == nil {
		return errors.New("the grant does not name the replica")
	}
	if err := r.PGReplica.Check(); err != nil {
		return err
	}
	if job == "replica-clone" {
		if _, err := netip.ParseAddr(r.PrimaryHost); err != nil && !hostName.MatchString(r.PrimaryHost) {
			return fmt.Errorf("not a host to copy from: %q", r.PrimaryHost)
		}
		if r.PrimaryPort <= 0 || r.PrimaryPort > 65535 {
			return errors.New("the grant does not say the primary's port")
		}
		if r.PrimaryVersion < 10 || r.PrimaryVersion > 99 {
			return errors.New("the grant does not say the primary's major version")
		}
		return nil
	}
	switch r.Op {
	case ReplicaCheck, ReplicaDrop:
	case ReplicaPrepare:
		if r.Verifier == "" {
			return errors.New("the grant carries no verifier for the replication role")
		}
	case ReplicaAllow:
		if r.Address == "" {
			return errors.New("the grant does not say the replica's address")
		}
	default:
		return fmt.Errorf("unknown replica operation %q", r.Op)
	}
	return nil
}

// PGReplicaCloned answers a clone.
type PGReplicaCloned struct {
	DataDir string `json:"dataDir"`
	// Kept is where the server's own, empty data directory went; Argus
	// leaves it for a person to remove.
	Kept       string `json:"kept"`
	Unit       string `json:"unit,omitempty"`
	Version    int    `json:"version"`
	InRecovery bool   `json:"inRecovery"`
	CopyMS     int64  `json:"copyMs"`
}
