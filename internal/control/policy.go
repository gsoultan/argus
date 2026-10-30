package control

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// GatewayPolicy is what a brokered session is permitted to do.
//
// Field names match web/src/types/domain.ts exactly, so the console and the
// control plane cannot drift into disagreeing about what a toggle is called.
//
// Every zero value is the *open* position for the forwarding fields and the
// *closed* one for the recording fields, which is why nothing here is a plain
// bool the caller can leave unset: see DefaultGatewayPolicy.
type GatewayPolicy struct {
	AllowLocalForward         bool `json:"allowLocalForward"`
	AllowRemoteForward        bool `json:"allowRemoteForward"`
	AllowAgentForward         bool `json:"allowAgentForward"`
	AllowX11Forward           bool `json:"allowX11Forward"`
	ProxySftpSubsystem        bool `json:"proxySftpSubsystem"`
	FailClosedOnRecordingLoss bool `json:"failClosedOnRecordingLoss"`
	RequireEbpfForRoot        bool `json:"requireEbpfForRoot"`

	// DirectSSH is what an agent does with a login that reached sshd without
	// the console: refuse it, hold it for an admin to approve, or allow it
	// and record it. A server can choose its own; see EffectiveDirectSSH.
	DirectSSH string `json:"directSsh"`

	// ElevatedPrincipals is served, never stored. It is what this control plane
	// enforces, sent so a gateway does not have to hold its own copy and drift.
	// Read-only from the console's point of view: a deployment that could edit
	// which principals are "elevated" could remove the approval requirement
	// from root by typing in a text box.
	ElevatedPrincipals []string `json:"elevatedPrincipals,omitempty"`

	UpdatedAt string `json:"updatedAt,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// DefaultGatewayPolicy is the safe position, and the one the gateway enforced
// by hardcoding it before this was configurable.
//
// Used whenever the stored policy cannot be read. That direction matters: a
// control plane that is down must not be able to open a channel, so the
// fallback is the closed configuration rather than the last one seen or an
// empty struct.
func DefaultGatewayPolicy() GatewayPolicy {
	return GatewayPolicy{
		AllowLocalForward:         false,
		AllowRemoteForward:        false,
		AllowAgentForward:         false,
		AllowX11Forward:           false,
		ProxySftpSubsystem:        true,
		FailClosedOnRecordingLoss: true,
		RequireEbpfForRoot:        true,
		DirectSSH:                 DirectSSHRefuse,
	}
}

/* ── Persistence ─────────────────────────────────────────────────────────── */

// GatewayPolicy reads the single policy row.
func (s *Store) GatewayPolicy(ctx context.Context) (GatewayPolicy, error) {
	var p GatewayPolicy
	err := s.pool.QueryRow(ctx, `
		SELECT allow_local_forward, allow_remote_forward, allow_agent_forward,
		       allow_x11_forward, proxy_sftp_subsystem, fail_closed_on_recording_loss,
		       require_ebpf_for_root, direct_ssh,
		       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), updated_by
		  FROM gateway_policy WHERE id = TRUE`).Scan(
		&p.AllowLocalForward, &p.AllowRemoteForward, &p.AllowAgentForward,
		&p.AllowX11Forward, &p.ProxySftpSubsystem, &p.FailClosedOnRecordingLoss,
		&p.RequireEbpfForRoot, &p.DirectSSH,
		&p.UpdatedAt, &p.UpdatedBy)
	if err != nil {
		return DefaultGatewayPolicy(), fmt.Errorf("read gateway policy: %w", err)
	}
	return p, nil
}

// SaveGatewayPolicy replaces the whole policy and returns what was stored.
//
// A whole-document write rather than a field patch: two admins editing at once
// cannot interleave into a combination neither of them chose. The second write
// wins entirely and visibly, instead of merging into a policy nobody authored.
func (s *Store) SaveGatewayPolicy(
	ctx context.Context, p GatewayPolicy, actor string,
) (GatewayPolicy, error) {
	if !validDirectSSH(p.DirectSSH) {
		return GatewayPolicy{}, ErrInvalidDirectSSH
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE gateway_policy SET
			allow_local_forward             = $1,
			allow_remote_forward            = $2,
			allow_agent_forward             = $3,
			allow_x11_forward               = $4,
			proxy_sftp_subsystem            = $5,
			fail_closed_on_recording_loss   = $6,
			require_ebpf_for_root           = $7,
			direct_ssh                      = $9,
			updated_at                      = now(),
			updated_by                      = $8
		WHERE id = TRUE`,
		p.AllowLocalForward, p.AllowRemoteForward, p.AllowAgentForward,
		p.AllowX11Forward, p.ProxySftpSubsystem, p.FailClosedOnRecordingLoss,
		p.RequireEbpfForRoot, actor, p.DirectSSH)
	if err != nil {
		return GatewayPolicy{}, fmt.Errorf("save gateway policy: %w", err)
	}
	return s.GatewayPolicy(ctx)
}

/* ── Change description ──────────────────────────────────────────────────── */

// policyField pairs a field with which direction loosens the gateway.
type policyField struct {
	label     string
	get       func(GatewayPolicy) bool
	riskyOn   bool
	rationale string
}

// policyFields drives both the audit detail and the risk classification, so a
// new toggle cannot be added to one and forgotten in the other.
var policyFields = []policyField{
	{"local port forwarding (-L)", func(p GatewayPolicy) bool { return p.AllowLocalForward }, true,
		"the gateway becomes a tunnel into the private network"},
	{"remote port forwarding (-R)", func(p GatewayPolicy) bool { return p.AllowRemoteForward }, true,
		"a target can open a listener back through the gateway"},
	{"SSH agent forwarding", func(p GatewayPolicy) bool { return p.AllowAgentForward }, true,
		"root on the gateway can sign challenges with the user's keys"},
	{"X11 forwarding", func(p GatewayPolicy) bool { return p.AllowX11Forward }, true,
		"broad attack surface, rarely needed for server administration"},
	{"SFTP subsystem proxying", func(p GatewayPolicy) bool { return p.ProxySftpSubsystem }, false,
		"file transfers stop producing per-file audit events"},
	{"fail closed on recording loss", func(p GatewayPolicy) bool { return p.FailClosedOnRecordingLoss }, false,
		"sessions may proceed unrecorded"},
	{"eBPF requirement for root sessions", func(p GatewayPolicy) bool { return p.RequireEbpfForRoot }, false,
		"root sessions may rely on PTY capture alone"},
}

// PolicyChange is one field that moved.
type PolicyChange struct {
	Label     string
	Enabled   bool
	Loosening bool
	Rationale string
	// Text describes a change that is not an on/off toggle, in place of
	// "enabled"/"disabled" and the label.
	Text string
}

// DiffGatewayPolicy reports what moved between two policies.
func DiffGatewayPolicy(before, after GatewayPolicy) []PolicyChange {
	var out []PolicyChange
	for _, f := range policyFields {
		was, now := f.get(before), f.get(after)
		if was == now {
			continue
		}
		out = append(out, PolicyChange{
			Label:     f.label,
			Enabled:   now,
			Loosening: now == f.riskyOn,
			Rationale: f.rationale,
		})
	}
	if before.DirectSSH != after.DirectSSH {
		out = append(out, PolicyChange{
			Label:     "direct SSH logins",
			Loosening: directSSHRank(after.DirectSSH) > directSSHRank(before.DirectSSH),
			Rationale: "logins that reach sshd without the console " + directSSHConsequence(after.DirectSSH),
			Text: "set direct SSH logins from " + directSSHLabel(before.DirectSSH) +
				" to " + directSSHLabel(after.DirectSSH),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Loosenings first: an auditor reading this entry cares about what was
		// opened, not what was tightened alongside it.
		if out[i].Loosening != out[j].Loosening {
			return out[i].Loosening
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// describePolicyChanges renders a diff as the audit log's detail string.
func describePolicyChanges(changes []PolicyChange) (detail string, loosened bool) {
	if len(changes) == 0 {
		return "Policy submitted with no effective change.", false
	}
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Text != "" {
			if c.Loosening {
				loosened = true
				parts = append(parts, c.Text+" — "+c.Rationale)
			} else {
				parts = append(parts, c.Text)
			}
			continue
		}
		verb := "disabled"
		if c.Enabled {
			verb = "enabled"
		}
		if c.Loosening {
			loosened = true
			parts = append(parts, fmt.Sprintf("%s %s — %s", verb, c.Label, c.Rationale))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", verb, c.Label))
	}
	return strings.Join(parts, "; ") + ".", loosened
}
