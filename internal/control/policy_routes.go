package control

import (
	"net/http"
)

// getPolicy serves the gateway policy to the console.
//
// Readable by any signed-in user, including auditors. Knowing whether agent
// forwarding is permitted is exactly the sort of thing an auditor is there to
// check, and a control they cannot see is one they cannot report on.
func (a *API) getPolicy(w http.ResponseWriter, r *http.Request, _ string) {
	p, err := a.store.GatewayPolicy(r.Context())
	if err != nil {
		a.fail(w, "gateway policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// postPolicy replaces the gateway policy.
//
// Owners and admins only, and always audited — including when nothing changed,
// because "someone opened the policy page and pressed save" is itself a fact an
// investigator may want. The response is what was stored rather than what was
// sent, so a console showing a field the control plane refused to change is not
// a state this can produce.
func (a *API) postPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden,
			"changing gateway policy requires the admin or owner role")
		return
	}

	var in GatewayPolicy
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed policy")
		return
	}

	// Read before write so the audit entry can say what actually moved rather
	// than restating the whole document, which is unreadable at review time.
	before, err := a.store.GatewayPolicy(r.Context())
	if err != nil {
		a.fail(w, "gateway policy", err)
		return
	}

	after, err := a.store.SaveGatewayPolicy(r.Context(), in, sess.Email)
	if err != nil {
		a.fail(w, "save gateway policy", err)
		return
	}

	changes := DiffGatewayPolicy(before, after)
	detail, loosened := describePolicyChanges(changes)
	severity := "notice"
	if loosened {
		// A loosening is the entry someone will be looking for after an
		// incident, so it is not filed at the same level as a tightening.
		severity = "warning"
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "policy.change",
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     "gateway",
		Detail:     detail,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	a.log.Info("gateway policy changed",
		"actor", sess.Email, "changes", len(changes), "loosened", loosened)
	writeJSON(w, http.StatusOK, after)
}

// getGatewayPolicy serves the policy to a gateway.
//
// Separate from the console route and behind the reporter credential, matching
// how every other machine-facing endpoint here is gated. A gateway holds a
// machine token and no browser session, so it could not reach the console route
// at all — and sharing one handler between the two audiences is how a browser
// eventually ends up able to read something meant for a gateway.
func (a *API) getGatewayPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GatewayPolicy(r.Context())
	if err != nil {
		// Deliberately an error rather than the default: a gateway that fetches
		// policy successfully must have fetched the real one. Handing it
		// defaults on a database fault would look identical to a healthy read
		// and would silently re-close a channel an admin had opened.
		a.fail(w, "gateway policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
