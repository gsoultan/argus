package control

import (
	"errors"
	"net/http"
	"strconv"
)

// getRequests lists access requests.
func (a *API) getRequests(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reqs, err := a.store.Requests(r.Context(), r.URL.Query().Get("state"), limit)
	if err != nil {
		a.fail(w, "requests", err)
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}

// postRequest creates one.
func (a *API) postRequest(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var in struct {
		AssetHostnames  []string `json:"assetHostnames"`
		Principal       string   `json:"principal"`
		Justification   string   `json:"justification"`
		DurationMinutes int      `json:"durationMinutes"`
		BreakGlass      bool     `json:"breakGlass"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// An auditor may read everything and request nothing — asking for access
	// they could never use only adds noise to the approval queue.
	if sess.Role == "auditor" {
		writeErr(w, http.StatusForbidden, "your role cannot request access")
		return
	}

	created, err := a.store.CreateRequest(r.Context(), AccessRequest{
		RequesterEmail:  sess.Email,
		AssetHostnames:  in.AssetHostnames,
		Principal:       in.Principal,
		Justification:   in.Justification,
		DurationMinutes: in.DurationMinutes,
		BreakGlass:      in.BreakGlass,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Break-glass is recorded at warning severity the moment it is asked for,
	// not only when it is used. The point of the path is that reaching for it
	// is visible.
	severity := "info"
	detail := "Requested " + in.Principal + " on " + strconv.Itoa(len(in.AssetHostnames)) + " host(s)."
	if in.BreakGlass {
		severity = "warning"
		detail = "BREAK-GLASS requested for " + in.Principal + ". " + in.Justification
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "request.create",
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     joinHosts(in.AssetHostnames),
		Detail:     detail,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	writeJSON(w, http.StatusOK, created)
}

// postDecision approves or denies a request.
func (a *API) postDecision(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Deciding is a privileged act in its own right. An operator who could
	// approve is an operator who holds standing root.
	switch sess.Role {
	case "approver", "admin", "owner":
	default:
		writeErr(w, http.StatusForbidden, "your role cannot decide access requests")
		return
	}

	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Decision != "approved" && in.Decision != "denied" {
		writeErr(w, http.StatusBadRequest, `decision must be "approved" or "denied"`)
		return
	}
	// A denial the requester cannot act on wastes everyone's time.
	if in.Decision == "denied" && len(in.Note) < 5 {
		writeErr(w, http.StatusBadRequest, "a denial must say why")
		return
	}

	out, err := a.store.DecideRequest(r.Context(), r.PathValue("id"),
		in.Decision, in.Note, sess.Email)
	if errors.Is(err, ErrSelfApproval) {
		a.log.Warn("self-approval attempt",
			"email", sess.Email, "request", r.PathValue("id"))
		// Worth an audit entry: someone reaching for this is worth knowing
		// about even though it was refused.
		if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "request.self_approval_blocked",
			Severity:   "warning",
			ActorEmail: sess.Email,
			Target:     "access-request",
			Detail:     "Attempted to decide their own access request. Refused.",
		}); aerr != nil {
			a.log.Error("audit append failed", "error", aerr)
		}
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, ErrAlreadyDecided) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	action := "request.approve"
	severity := "notice"
	detail := "Approved " + out.Principal + " for " + out.RequesterEmail +
		", expiring " + out.ExpiresAt.Format("15:04:05 MST") + "."
	if in.Decision == "denied" {
		action = "request.deny"
		detail = "Denied " + out.Principal + " for " + out.RequesterEmail + ": " + in.Note
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     action,
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     joinHosts(out.AssetHostnames),
		Detail:     detail,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	a.log.Info("access request decided",
		"decision", in.Decision, "by", sess.Email,
		"requester", out.RequesterEmail, "principal", out.Principal)
	writeJSON(w, http.StatusOK, out)
}

// grantFor reports the caller's active grant, so the console can tell someone
// why they cannot connect and how long they have once they can.
func (a *API) getGrant(w http.ResponseWriter, r *http.Request, email string) {
	host := r.URL.Query().Get("target")
	principal := r.URL.Query().Get("principal")
	if host == "" || principal == "" {
		writeErr(w, http.StatusBadRequest, "target and principal are required")
		return
	}
	ok, expires, err := a.store.ActiveGrant(r.Context(), email, host, principal)
	if err != nil {
		a.fail(w, "grant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granted":   ok,
		"expiresAt": expires,
	})
}

func joinHosts(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	if len(hosts) == 1 {
		return hosts[0]
	}
	return hosts[0] + " +" + strconv.Itoa(len(hosts)-1) + " more"
}
