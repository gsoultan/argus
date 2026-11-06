package agent

import "strings"

// SessionReport is a session as the control plane's report/session route
// takes it: what the daemon sends for every session it records, and the
// Argus app for Android for every device session, so a session is the same
// row whichever recorded it.
func SessionReport(rec SessionRecord) map[string]any {
	// A session through the tunnel is attributed to the person the control
	// plane signed it for; a login through sshd only to the local account.
	who := rec.Principal + "@" + rec.Hostname
	if rec.Actor != "" {
		who = rec.Actor
	}
	protocol, state := rec.Protocol, rec.State
	if protocol == "" {
		protocol = "ssh"
	}
	if state == "" {
		state = "closed"
	}
	fidelity := rec.Fidelity
	if protocol == "rdp" {
		fidelity = "rdp"
	}
	out := map[string]any{
		"id":             rec.ID,
		"userEmail":      who,
		"assetHostname":  rec.Hostname,
		"principal":      rec.Principal,
		"protocol":       protocol,
		"origin":         string(rec.Origin),
		"state":          state,
		"startedAt":      rec.StartedAt,
		"clientIp":       rec.ClientAddr,
		"fidelity":       fidelity,
		"commandCount":   rec.Commands,
		"recordingBytes": rec.Bytes,
		"reportedBy":     "agent:" + rec.Hostname,
		"riskFlags":      riskFlags(rec),
	}
	if state != "active" {
		out["endedAt"] = rec.EndedAt
		out["exitCode"] = rec.ExitCode
		out["chainHead"] = rec.ChainHead
	}
	if rec.TerminatedBy != "" {
		out["terminatedBy"] = rec.TerminatedBy
		out["terminationReason"] = rec.TerminationReason
	}
	if rec.Auth != nil {
		out["auth"] = rec.Auth
	}
	if rec.DirectApproval != "" {
		out["directApproval"] = rec.DirectApproval
		out["directLoginId"] = rec.DirectLoginID
	}
	return out
}

// riskFlags marks what the console highlights about a session the agent saw.
func riskFlags(rec SessionRecord) []string {
	flags := []string{}
	if rec.Origin == Direct {
		flags = append(flags, "bypassed-gateway")
	}
	// Administrator is to Windows what root is to Linux.
	switch strings.ToLower(rec.Principal) {
	case "root", "administrator":
		flags = append(flags, "root-principal")
	}
	return append(flags, rec.Flags...)
}
