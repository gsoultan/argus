package control

import (
	"errors"
	"net/http"
	"strings"
)

// getCoverage answers "does Argus actually see everything?".
func (a *API) getCoverage(w http.ResponseWriter, r *http.Request, _ string) {
	c, err := a.store.Coverage(r.Context())
	if err != nil {
		a.fail(w, "coverage", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// getDiscovered lists agents reporting from outside the inventory.
func (a *API) getDiscovered(w http.ResponseWriter, r *http.Request, _ string) {
	state := r.URL.Query().Get("state")
	switch state {
	case "", "unreviewed", "enrolled", "ignored":
	default:
		writeErr(w, http.StatusBadRequest, "state must be unreviewed, enrolled or ignored")
		return
	}
	hosts, err := a.store.DiscoveredHosts(r.Context(), state)
	if err != nil {
		a.fail(w, "discovered hosts", err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

// postEnrol promotes a discovered host into a managed asset.
//
// Admins and owners only. Enrolment creates something the access-request
// machinery then grants against, so it is a change to what Argus manages rather
// than a note about it.
func (a *API) postEnrol(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "enrolling a host requires the admin or owner role")
		return
	}
	hostname := r.PathValue("hostname")
	if hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}

	id, err := a.store.EnrolHost(r.Context(), hostname, sess.Email)
	if errors.Is(err, ErrAlreadyReviewed) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "enrol host", err)
		return
	}

	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "asset.enrolled",
		Severity:   "info",
		ActorEmail: sess.Email,
		Target:     hostname,
		Detail: "Enrolled discovered host " + hostname + " as a managed asset. " +
			"No principals were granted; they must be added deliberately.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	a.log.Info("host enrolled", "hostname", hostname, "actor", sess.Email, "asset", id)
	writeJSON(w, http.StatusOK, map[string]any{"assetId": id, "hostname": hostname})
}

// postIgnoreHost records a deliberate decision not to manage a host.
func (a *API) postIgnoreHost(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "dismissing a host requires the admin or owner role")
		return
	}
	hostname := r.PathValue("hostname")
	if hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}

	var in struct {
		Note string `json:"note"`
	}
	_ = decode(r, &in)
	note := strings.TrimSpace(in.Note)
	if note == "" {
		// A dismissal with no reason is indistinguishable from a host nobody
		// looked at, and leaves the next reviewer to redo the work.
		writeErr(w, http.StatusBadRequest,
			"a note is required: say why this host is deliberately unmanaged")
		return
	}

	err := a.store.IgnoreHost(r.Context(), hostname, sess.Email, note)
	if errors.Is(err, ErrAlreadyReviewed) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "ignore host", err)
		return
	}

	// Warning severity: choosing to leave a privileged host unmanaged is a
	// decision someone should be able to find later and question.
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "asset.dismissed",
		Severity:   "warn",
		ActorEmail: sess.Email,
		Target:     hostname,
		Detail:     "Marked " + hostname + " as deliberately unmanaged: " + note,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	a.log.Warn("host dismissed from coverage",
		"hostname", hostname, "actor", sess.Email, "note", note)
	writeJSON(w, http.StatusOK, map[string]string{"hostname": hostname, "state": "ignored"})
}

// postFacts records what an agent reports about its host. Machine-to-machine.
func (a *API) postFacts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname string `json:"hostname"`
		HostFacts
	}
	if err := decodeReport(r, &in, a.log, "report/facts"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if err := a.store.RecordFacts(r.Context(), in.Hostname, in.HostFacts); err != nil {
		a.fail(w, "record facts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
