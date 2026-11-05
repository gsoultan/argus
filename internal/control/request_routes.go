package control

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// getRequests lists access requests: the whole queue for whoever decides or
// audits them, and only their own for everyone else. The queue carries other
// people's addresses, hosts and reasons, which an operator asking for a
// server has no business reading.
func (a *API) getRequests(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	requester := ""
	var edited []string
	if !seesRequestQueue(sess.Role) {
		requester = sess.Email
		// Their own, and the changes on databases whose editors approve
		// each other's, which they edit.
		var err error
		if sess.Role != "auditor" {
			if edited, err = a.store.EditedDatabases(r.Context(), sess.Email); err != nil {
				a.fail(w, "requests", err)
				return
			}
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reqs, err := a.store.Requests(r.Context(), r.URL.Query().Get("state"), requester, limit, edited...)
	if err != nil {
		a.fail(w, "requests", err)
		return
	}
	if err := a.hidePreviews(r.Context(), sess.Email, sess.Role, reqs); err != nil {
		a.fail(w, "requests", err)
		return
	}
	if len(edited) > 0 {
		for i := range reqs {
			reqs[i].AsEditor = !strings.EqualFold(reqs[i].RequesterEmail, sess.Email)
		}
	}
	writeJSON(w, http.StatusOK, reqs)
}

// seesRequestQueue is whether role reads everyone's requests: the roles that
// may decide them, and the auditor who reads everything.
func seesRequestQueue(role string) bool {
	return role == "approver" || seesWholeFleet(role)
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

	// An approval for an account the server does not have is a grant nobody
	// can use. Refused here, because the form is not the boundary.
	if len(in.AssetHostnames) > maxRequestHosts {
		writeErr(w, http.StatusBadRequest,
			"ask for at most "+strconv.Itoa(maxRequestHosts)+" servers at once")
		return
	}
	hosts, err := a.store.RequestableAccounts(r.Context(), dedupe(in.AssetHostnames))
	if err != nil {
		a.fail(w, "request accounts", err)
		return
	}
	if why := unrequestable(hosts, in.Principal); why != "" {
		writeErr(w, http.StatusBadRequest, why)
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
	a.alertRequestWaiting(r.Context(), created)

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
	asEditor := ""
	switch sess.Role {
	case "approver", "admin", "owner":
	default:
		// Or an editor of the database a change is for, where its editors
		// approve each other's changes.
		db, ok, err := a.decidesAsEditor(r.Context(), sess.Email, r.PathValue("id"))
		if errors.Is(err, ErrRequestNotFound) {
			writeErr(w, http.StatusNotFound, "no such request; it may have been removed")
			return
		}
		if err != nil {
			a.fail(w, "decide", err)
			return
		}
		if !ok || sess.Role == "auditor" {
			writeErr(w, http.StatusForbidden, "your role cannot decide access requests")
			return
		}
		asEditor = db.Name
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
		// Someone else decided it, or the person who asked took it back,
		// while it was on this approver's screen. Their alert is stale too.
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrRequestNotFound) {
		writeErr(w, http.StatusNotFound, "no such request; it may have been removed")
		return
	}
	if err != nil {
		a.fail(w, "decide", err)
		return
	}

	// Worked out per decision: a denial has no expiry. Building the
	// approval's words first dereferenced it and crashed every denial after
	// it had committed, which a proxy in front answered as a 502.
	action, severity := "request.approve", "notice"
	var detail string
	if in.Decision == "denied" {
		action = "request.deny"
		detail = "Denied " + out.Principal + " for " + out.RequesterEmail + ": " + in.Note
	} else {
		detail = "Approved " + out.Principal + " for " + out.RequesterEmail + "."
		if out.ExpiresAt != nil {
			detail = "Approved " + out.Principal + " for " + out.RequesterEmail +
				", expiring " + out.ExpiresAt.UTC().Format("15:04:05 MST") + "."
		}
	}
	if asEditor != "" {
		detail += " Decided as an editor of " + asEditor + "."
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

	a.closeRequestAlerts(r.Context(), out)
	a.alertRequestDecided(r.Context(), out, in.Decision, sess.Email, in.Note)
	a.log.Info("access request decided",
		"decision", in.Decision, "by", sess.Email,
		"requester", out.RequesterEmail, "principal", out.Principal)
	// A request that stands for a database restore in place runs it now.
	if out.Principal == "db:restore" {
		if in.Decision == "approved" {
			a.restoreApproved(r.Context(), out.ID, sess.Email)
		} else {
			a.restoreDenied(r.Context(), out.ID, sess.Email, in.Note)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// postCancel takes back a pending request, for the person who asked.
//
// Before this existed, closing the console's wait only stopped the console
// waiting: the request stayed pending in every approver's queue and bell,
// and an approval given later was one nobody would use.
func (a *API) postCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	out, err := a.store.CancelRequest(r.Context(), r.PathValue("id"), sess.Email, clip(strings.TrimSpace(in.Note), 500))
	switch {
	case errors.Is(err, ErrRequestNotFound):
		writeErr(w, http.StatusNotFound, "no such request")
		return
	case errors.Is(err, ErrNotYourRequest):
		writeErr(w, http.StatusForbidden, err.Error()+"; an approver who does not want it denies it")
		return
	case errors.Is(err, ErrAlreadyDecided):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "cancel request", err)
		return
	}
	detail := "Cancelled their request for " + describeAsk(out.Principal, out.AssetHostnames) + " before it was decided."
	if out.DecisionNote != nil {
		detail += " " + *out.DecisionNote
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "request.cancel",
		Severity:   "info",
		ActorEmail: sess.Email,
		Target:     joinHosts(out.AssetHostnames),
		Detail:     detail + " (request " + out.ID + ")",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	a.closeRequestAlerts(r.Context(), out)
	if out.Principal == "db:restore" {
		a.restoreCancelled(r.Context(), out.ID, sess.Email)
	}
	if out.Principal == statementPrincipal {
		if asks, err := a.store.StatementAsks(r.Context(), []string{out.ID}); err == nil {
			out.Statement = asks[out.ID]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getOneRequest reads one request, for whoever may see it in the list: the
// queue's readers, the person who asked, and an editor deciding a change on
// a database they edit. Anyone else is told it does not exist.
//
// An alert links to its request; by the time it is opened the request may
// be decided or taken back, and the page has to say so rather than show
// nothing.
func (a *API) getOneRequest(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req, dbID, err := a.store.Request(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrRequestNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		a.fail(w, "request", err)
		return
	}
	mine := strings.EqualFold(req.RequesterEmail, sess.Email)
	if !seesRequestQueue(sess.Role) && !mine {
		edits := false
		if sess.Role != "auditor" && req.Principal == statementPrincipal && dbID != "" {
			edited, err := a.store.EditedDatabases(r.Context(), sess.Email)
			if err != nil {
				a.fail(w, "request", err)
				return
			}
			edits = slices.Contains(edited, dbID)
		}
		if !edits {
			writeErr(w, http.StatusNotFound, "no such request")
			return
		}
		req.AsEditor = true
	}
	one := []AccessRequest{req}
	if err := a.hidePreviews(r.Context(), sess.Email, sess.Role, one); err != nil {
		a.fail(w, "request", err)
		return
	}
	writeJSON(w, http.StatusOK, one[0])
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
