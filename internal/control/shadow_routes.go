package control

import (
	"net/http"

	"github.com/gsoultan/argus/internal/auth"
)

// handleShadowTicket authorises watching a session already in flight.
//
// Watching a colleague work as root is surveillance of a person, so it is
// deliberately not something an ordinary user can do to another. Auditors can,
// because observing privileged access is the entire point of the role — note
// that they are refused a terminal of their own in handleTicket, which is the
// same separation seen from the other side.
func (a *API) handleShadowTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "session id is required")
		return
	}

	subject, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "load session", err)
		return
	}
	if subject == nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}

	// Your own session is always yours to watch; nothing is disclosed that you
	// are not already looking at.
	if subject.UserEmail != sess.Email {
		switch sess.Role {
		case "admin", "owner", "auditor":
		default:
			writeErr(w, http.StatusForbidden,
				"watching another user's session requires the auditor, admin or owner role")
			return
		}
	}

	ticket, err := a.signer.IssueSessionScopedTicket(
		sess.Email, auth.ScopeShadow, id, a.ticketTTL())
	if err != nil {
		a.fail(w, "issue shadow ticket", err)
		return
	}

	// Audited before the ticket is handed over, and at warning severity when it
	// is somebody else's session. "Who watched whom" is exactly the question a
	// privileged-access review has to be able to answer about the people
	// operating the system itself.
	severity, detail := "info", "Attached a live view to their own session."
	if subject.UserEmail != sess.Email {
		severity = "warn"
		detail = "Attached a live view to " + subject.UserEmail + "'s session as " +
			subject.Principal + "@" + subject.AssetHostname + "."
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "session.shadow",
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     id,
		Detail:     detail,
	}); aerr != nil {
		// The audit trail is the product. Issuing a ticket we could not record
		// would mean somebody watched a privileged session with no evidence
		// that they did.
		a.fail(w, "audit shadow", aerr)
		return
	}

	a.log.Info("shadow ticket issued",
		"viewer", sess.Email, "session", id, "subject", subject.UserEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    ticket,
		"session":   id,
		"expiresIn": int(a.ticketTTL().Seconds()),
	})
}

// handleTerminateTicket authorises ending a session already in flight.
//
// Admins and owners only, plus anyone ending their own session. Auditors are
// excluded on purpose: the role's value is that it observes without the power
// to alter what it observes, and an auditor who can cut a session short can
// shape the evidence they are there to review.
func (a *API) handleTerminateTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "session id is required")
		return
	}

	var in struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &in)
	if in.Reason == "" {
		writeErr(w, http.StatusBadRequest,
			"a reason is required; a session cut short with no explanation is a gap in the record")
		return
	}

	subject, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "load session", err)
		return
	}
	if subject == nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}

	if subject.UserEmail != sess.Email {
		switch sess.Role {
		case "admin", "owner":
		default:
			writeErr(w, http.StatusForbidden,
				"ending another user's session requires the admin or owner role")
			return
		}
	}

	ticket, err := a.signer.IssueSessionScopedTicket(
		sess.Email, auth.ScopeTerminate, id, a.ticketTTL())
	if err != nil {
		a.fail(w, "issue terminate ticket", err)
		return
	}

	// Recorded at the point of authorisation rather than after the gateway
	// acts. If the gateway is unreachable the attempt still belongs in the log:
	// an operator who tried to stop a session and could not is a more urgent
	// finding than one who succeeded.
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "session.terminate",
		Severity:   "warn",
		ActorEmail: sess.Email,
		Target:     id,
		Detail: "Authorised ending " + subject.UserEmail + "'s session as " +
			subject.Principal + "@" + subject.AssetHostname + ": " + in.Reason,
	}); aerr != nil {
		a.fail(w, "audit terminate", aerr)
		return
	}

	a.log.Warn("terminate ticket issued",
		"actor", sess.Email, "session", id, "subject", subject.UserEmail, "reason", in.Reason)
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    ticket,
		"session":   id,
		"reason":    in.Reason,
		"expiresIn": int(a.ticketTTL().Seconds()),
	})
}
