package control

import (
	"net/http"
	"time"
)

/*
Deciding whether a session may open as an elevated principal.

The console has always said an elevated principal needs an approved access
request, and handleTicket enforced that for browser terminals. The SSH gateway
-- the primary path, on a product that leads with "Linux SSH first" -- enforced
nothing. It checked authorized_keys and the asset's principal list, then dialled
the target as root. Where a key was injected for root, which is the whole point
of credential injection, that session opened.

So the control that a customer buys this for was enforced on the path they use
least. This endpoint is what the gateway asks, and it is deliberately the
control plane that decides: the rules for who counts as elevated, who is exempt
and what an approval means live in one place, or they drift.
*/

// Authorization is the answer to "may this person open this principal here?".
type Authorization struct {
	Allowed bool `json:"allowed"`
	// Reason is shown to the operator on a refusal, so it has to say what to do
	// next rather than only that the answer was no.
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// KernelEvidenceWaived means the policy demanded kernel-observed execution
	// evidence for this session and it was allowed without any, because the
	// requester's role exempts them. The session is real and permitted; what it
	// cannot do is evidence what ran, and the console says so rather than
	// showing an elevated session that looks like every other one.
	KernelEvidenceWaived bool `json:"kernelEvidenceWaived,omitempty"`
}

// handleAuthorize answers a gateway asking about one session.
//
// Reporter-authenticated: the caller is a gateway holding a client certificate,
// not a person, so the subject's address is a parameter rather than something
// read from a session cookie.
func (a *API) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	target := r.URL.Query().Get("target")
	principal := r.URL.Query().Get("principal")
	if email == "" || target == "" || principal == "" {
		writeErr(w, http.StatusBadRequest, "email, target and principal are required")
		return
	}

	auth, err := a.authorizePrincipal(r, email, target, principal)
	if err != nil {
		a.fail(w, "authorize", err)
		return
	}
	writeJSON(w, http.StatusOK, auth)
}

// kernelEvidenceWaived reports whether policy demanded kernel-observed evidence
// for a session on this target and the target cannot produce it.
//
// Only the role-exempt path asks. Everyone else is refused by the block below,
// so for them the question never becomes a waiver.
//
// An error reading either the policy or the agent's capability is not a waiver:
// it is not knowing, and claiming a waiver that was never granted would put a
// false line in the audit chain. The refusal path treats an error as fatal for
// the same reason from the other direction.
func (a *API) kernelEvidenceWaived(r *http.Request, target string) (bool, string) {
	pol, err := a.store.GatewayPolicy(r.Context())
	if err != nil || !pol.RequireEbpfForRoot {
		return false, ""
	}
	tracing, why, err := a.store.ExecTracingFor(r.Context(), target)
	if err != nil || tracing {
		return false, ""
	}
	if why == "" {
		why = "the agent reports no kernel probe"
	}
	return true, why
}

// authorizePrincipal decides one session: assignment first, then elevation.
//
// The gateway asks when its own synced inventory does not already show the
// person assigned, and always for an elevated principal. So this is the single
// place both paths get their answer from, which is the point: the rules for who
// may reach a host live here, or the browser and ssh(1) drift into two products
// with two different ideas of who is allowed.
func (a *API) authorizePrincipal(r *http.Request, email, target, principal string) (Authorization, error) {
	elevated := isElevated(principal)

	// Admins are exempt, as they are on the browser path, because someone has
	// to be able to act when the approval chain itself is broken. Their
	// sessions are recorded and risk-flagged like everyone else's, and the
	// exemption is audited here rather than left implicit.
	if acct, err := a.store.Account(r.Context(), email); err == nil {
		if acct.Role == "admin" || acct.Role == "owner" {
			if !elevated {
				return Authorization{Allowed: true}, nil
			}
			detail := "Allowed a session as " + principal + " by role " + acct.Role +
				", with no access request. Admins are exempt so the approval " +
				"chain being broken cannot lock everyone out."
			// The exemption was written for the approval chain, and it clears
			// the kernel-evidence requirement on its way past. Extending it
			// there is deliberate -- an admin sent to repair a broken agent
			// cannot be blocked by that agent being broken -- but it returned
			// before the requirement was even read, so the record said only
			// that a role allowed the session. An auditor had to infer the
			// waiver from the absence of a refusal.
			waived, why := a.kernelEvidenceWaived(r, target)
			if waived {
				a.log.Warn("elevated session allowed by role without kernel evidence",
					"email", email, "role", acct.Role, "principal", principal,
					"target", target, "reason", why)
				detail += " Policy requires kernel-observed execution evidence for " +
					principal + " sessions and " + why + ". The requirement was " +
					"waived by role: this session is recorded at PTY fidelity and " +
					"cannot evidence what ran."
			} else {
				a.log.Info("elevated session allowed by role",
					"email", email, "role", acct.Role, "principal", principal, "target", target)
			}
			a.auditElevation(r, email, target, detail)
			return Authorization{Allowed: true, KernelEvidenceWaived: waived}, nil
		}
	}

	// An ordinary principal is decided by assignment.
	//
	// Elevated ones fall through to the rules below instead, where an approved
	// access request is required outright — a grant names one person, one host
	// and one principal and expires, so it is strictly narrower than an
	// assignment and there is nothing an assignment could add. Checking
	// assignment first would also move the refusal, and with it the audit
	// entry, away from the reason the session was actually refused.
	if !elevated {
		return a.authorizeAssignment(r, email, target, principal)
	}

	// Kernel evidence, when the policy insists on it for elevated sessions.
	//
	// "PTY capture alone can be defeated by base64 or by running a script" is
	// what the console says about this switch, and it was true of every root
	// session because nothing read the switch. A session recorded at PTY
	// fidelity cannot evidence what ran, which for root is the whole question.
	if pol, perr := a.store.GatewayPolicy(r.Context()); perr == nil && pol.RequireEbpfForRoot {
		tracing, why, terr := a.store.ExecTracingFor(r.Context(), target)
		if terr != nil {
			return Authorization{}, terr
		}
		if !tracing {
			if why == "" {
				why = "the agent reports no kernel probe"
			}
			a.log.Warn("elevated session refused: no kernel evidence available",
				"email", email, "principal", principal, "target", target, "reason", why)
			if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
				Action:     "session.elevation_refused",
				Severity:   "notice",
				ActorEmail: email,
				Target:     target,
				Detail: "Refused a session as " + principal +
					": policy requires kernel-observed execution evidence and " + why + ".",
			}); aerr != nil {
				a.log.Error("audit append failed", "error", aerr)
			}
			return Authorization{Reason: "policy requires kernel-observed evidence for " +
				principal + " sessions, and " + why +
				"; install or repair the Argus agent on this host, or change the policy"}, nil
		}
	}

	granted, expires, err := a.store.ActiveGrant(r.Context(), email, target, principal)
	if err != nil {
		return Authorization{}, err
	}
	if !granted {
		a.log.Warn("elevated session refused: no active grant",
			"email", email, "principal", principal, "target", target)
		if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "session.elevation_refused",
			Severity:   "notice",
			ActorEmail: email,
			Target:     target,
			Detail: "Refused a session as " + principal +
				": no approved access request covers it.",
		}); aerr != nil {
			a.log.Error("audit append failed", "error", aerr)
		}
		// Say what to do about it. A refusal with no next step is a support
		// ticket rather than a control.
		return Authorization{Reason: "opening a session as " + principal +
			" needs an approved access request; request one and have an approver decide it"}, nil
	}

	a.log.Info("elevated session authorised by grant",
		"email", email, "principal", principal, "target", target, "expires", expires)
	// The positive case, not only the refusals.
	//
	// "Who held root on this host, when, and under which approval" is the
	// question an audit opens with, and it was the one event this path did not
	// record -- every refusal was in the chain and every grant was not. The
	// browser terminal has audited its own authorisations all along; this is
	// the same fact arriving by the other door.
	until := "an unstated time"
	if expires != nil {
		until = expires.UTC().Format(time.RFC3339)
	}
	a.auditElevation(r, email, target,
		"Allowed a session as "+principal+" under an approved access request, until "+until+".")
	return Authorization{Allowed: true, ExpiresAt: expires}, nil
}

// authorizeAssignment decides an ordinary principal.
//
// An approved access request counts as well as an assignment: it is narrower —
// one host, one principal, and it expires — so a product that refused it here
// would have no just-in-time access at all, and the only answer to "I need this
// host for an hour" would be a permanent assignment.
//
// The refusal is audited. Unlike the console, this endpoint is reached only by
// a gateway holding the machine credential, so the entries cannot be produced
// at will by anyone off the network, and "X was refused Y on Z" is exactly what
// an investigation is looking for.
func (a *API) authorizeAssignment(
	r *http.Request, email, target, principal string,
) (Authorization, error) {
	assigned, err := a.store.AssignmentAllows(r.Context(), email, target, principal)
	if err != nil {
		return Authorization{}, err
	}
	if assigned {
		return Authorization{Allowed: true}, nil
	}

	granted, expires, err := a.store.ActiveGrant(r.Context(), email, target, principal)
	if err != nil {
		return Authorization{}, err
	}
	if granted {
		a.log.Info("session authorised by grant rather than assignment",
			"email", email, "principal", principal, "target", target, "expires", expires)
		return Authorization{Allowed: true, ExpiresAt: expires}, nil
	}

	a.log.Warn("session refused: not assigned",
		"email", email, "principal", principal, "target", target)
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "session.refused_unassigned",
		Severity:   "notice",
		ActorEmail: email,
		Target:     target,
		Detail: "Refused a session as " + principal +
			": this host is not assigned to them and no approved access request covers it.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	return Authorization{Reason: email + " is not assigned " + target + " as " + principal +
		"; an administrator assigns a host, or an approved access request covers it temporarily"}, nil
}

// auditElevation records that elevated access was granted.
func (a *API) auditElevation(r *http.Request, email, target, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "session.elevation_allowed",
		// Notice, not info: this is the line an auditor searches for, and it
		// should not sit at the same level as routine chatter.
		Severity:   "notice",
		ActorEmail: email,
		Target:     target,
		Detail:     detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}
