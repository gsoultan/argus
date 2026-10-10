package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"
)

// agentHostKey carries the hostname an agent credential is bound to.
const agentHostKey ctxKey = "argus.agent-host"

// EnrollTrust is what an agent needs to trust this control plane before it
// has anything else: a pin it checks during the one enrollment request, and
// the CA it keeps for every request after.
type EnrollTrust struct {
	// Pin returns "sha256:<hex>" of the serving certificate's public key, or ""
	// when this process does not terminate TLS itself (a proxy in front
	// presents some other key, and a pin of ours would never match).
	Pin func() (string, error)
	// CAPEM is handed to the agent on enrollment so it can verify the control
	// plane after the pin has done its one job. Public; empty when the agent
	// should use the system's trust store.
	CAPEM []byte
}

// agentHost returns the hostname of the agent credential that authenticated r,
// or "" when it was the shared reporter token.
func agentHost(r *http.Request) string {
	if v, ok := r.Context().Value(agentHostKey).(string); ok {
		return v
	}
	return ""
}

// agentReporter gates the three routes an agent reports to: session, heartbeat
// and facts. It accepts the shared reporter token exactly as reporter does, or
// a per-host agent credential.
//
// An agent credential does not also need a client certificate. The certificate
// is there so a leaked shared token is not enough on its own; a per-host
// credential already names one host, can only speak for it, and is revoked on
// its own -- the property the certificate was standing in for. Requiring both
// would make enrolling a host a certificate-issuing exercise again, which is
// the step this exists to remove. A certificate that is offered is still
// verified by the TLS layer.
func (a *API) agentReporter(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	shared := a.reporter(next)
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !strings.HasPrefix(tok, agentCredentialPrefix) {
			shared(w, r)
			return
		}
		host, ok := a.lookupAgentCredential(r.Context(), tok)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentHostKey, host)))
	}
}

// lookupAgentCredential resolves a credential to its host, through the cache.
func (a *API) lookupAgentCredential(ctx context.Context, tok string) (string, bool) {
	hash := hashSecret(tok)
	now := time.Now()
	if e, ok := a.agentCreds.get(hash, now); ok {
		return e.hostname, true
	}
	id, host, err := a.store.agentCredentialHost(ctx, hash)
	if err != nil {
		// Unknown, revoked, or the database is unreachable. All three refuse:
		// a reporter that cannot be identified does not get to write evidence.
		return "", false
	}
	a.agentCreds.put(hash, agentCredEntry{id: id, hostname: host, checked: now})
	return host, true
}

// mayReportFor refuses a report an agent credential makes about another host.
// The shared token may still report for any host; that is what gateways do.
func (a *API) mayReportFor(w http.ResponseWriter, r *http.Request, hostname string) bool {
	bound := agentHost(r)
	if bound == "" || strings.EqualFold(bound, hostname) {
		return true
	}
	a.log.Warn("agent credential reported for a host it is not bound to",
		"credential_host", bound, "reported_host", hostname, "remote", r.RemoteAddr)
	writeErr(w, http.StatusForbidden,
		"this agent is enrolled as "+bound+" and may only report for that host")
	return false
}

/* ── Console: minting, listing, revoking ─────────────────────────────────── */

// postEnrollToken mints a token for one agent install. Admin or owner.
func (a *API) postEnrollToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "adding a server requires the admin or owner role")
		return
	}
	var in struct {
		Hostname   string `json:"hostname"`
		TTLMinutes int    `json:"ttlMinutes"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	tok, err := a.store.CreateEnrollToken(r.Context(), sess.Email, in.Hostname,
		time.Duration(in.TTLMinutes)*time.Minute)
	if err != nil {
		a.fail(w, "create enrollment token", err)
		return
	}

	target, scope := tok.Hostname, "for "+tok.Hostname+" only"
	if target == "" {
		target, scope = "any host", "for any hostname"
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "agent.enroll_token.created",
		Severity:   "info",
		ActorEmail: sess.Email,
		Target:     target,
		Detail: "Made a single-use agent enrollment token " + scope + ", valid until " +
			tok.ExpiresAt.UTC().Format(time.RFC3339) + " (token " + tok.ID + ").",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	out := map[string]any{
		"id": tok.ID, "token": tok.Token, "hostname": tok.Hostname,
		"expiresAt": tok.ExpiresAt,
	}
	if a.Enroll.Pin != nil {
		if pin, perr := a.Enroll.Pin(); perr != nil {
			a.log.Error("could not compute the control plane's certificate pin", "error", perr)
		} else if pin != "" {
			out["pin"] = pin
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getAgentCredentials lists enrolled agents. Admin or owner: it is a list of
// every host able to write evidence.
func (a *API) getAgentCredentials(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "listing enrolled agents requires the admin or owner role")
		return
	}
	creds, err := a.store.AgentCredentials(r.Context())
	if err != nil {
		a.fail(w, "list agent credentials", err)
		return
	}
	writeJSON(w, http.StatusOK, creds)
}

// postRevokeAgent withdraws one agent's credential. Its reports stop being
// accepted; nothing it reported before is touched.
func (a *API) postRevokeAgent(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "revoking an agent requires the admin or owner role")
		return
	}
	id := r.PathValue("id")
	hostname, err := a.store.RevokeAgentCredential(r.Context(), id, sess.Email)
	if errors.Is(err, ErrAgentCredentialNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "revoke agent credential", err)
		return
	}
	a.agentCreds.forget(id)

	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "agent.revoked",
		Severity:   "warn",
		ActorEmail: sess.Email,
		Target:     hostname,
		Detail: "Revoked the agent credential for " + hostname + " (" + id + "). " +
			"Its reports are refused from now on; what it reported before is unchanged.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	a.log.Warn("agent credential revoked", "hostname", hostname, "actor", sess.Email)
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "hostname": hostname, "state": "revoked"})
}

/* ── Fleet: the exchange itself ──────────────────────────────────────────── */

// postEnroll exchanges an enrollment token for a per-host credential.
//
// Unauthenticated except by the token, which is the point: the host has no
// other credential yet. Throttled as a sign-in is, and every refusal charges
// the failure budget, so the endpoint cannot be used to guess tokens even
// though 256 bits already makes that hopeless.
func (a *API) postEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Hostname string `json:"hostname"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A shared reporter token pasted where an enrollment token belongs. Said
	// plainly, because the console used to tell people to paste exactly that.
	if a.ReporterToken != "" && subtle.ConstantTimeCompare(
		[]byte(in.Token), []byte(a.ReporterToken)) == 1 {
		writeErr(w, http.StatusBadRequest,
			"that is the gateways' reporter token, not an enrollment token; "+
				"make one under Servers > Add a server")
		return
	}

	enr, err := a.store.Enroll(r.Context(), in.Token, in.Hostname)
	switch {
	case errors.Is(err, ErrHostAlreadyEnrolled):
		// Not charged as a failure: the token was good, and someone
		// re-installing a host should be able to revoke and retry.
		a.log.Warn("enrollment refused: host already enrolled",
			"hostname", in.Hostname, "remote", r.RemoteAddr)
		writeErr(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrEnrollTokenInvalid), errors.Is(err, ErrEnrollTokenExpired),
		errors.Is(err, ErrEnrollTokenUsed), errors.Is(err, ErrEnrollTokenWrongHost):
		a.RecordAuthFailure(r)
		a.log.Warn("enrollment refused", "hostname", in.Hostname,
			"remote", r.RemoteAddr, "reason", err.Error())
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	case err != nil && strings.Contains(err.Error(), "hostname is required"):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		a.fail(w, "enroll agent", err)
		return
	}
	a.RecordAuthSuccess(r)

	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "agent.enrolled",
		Severity:   "info",
		ActorEmail: enr.TokenCreatedBy,
		Target:     in.Hostname,
		Detail: "An agent on " + in.Hostname + " enrolled from " + r.RemoteAddr +
			" with a token made by " + enr.TokenCreatedBy + " (token " + enr.TokenID +
			", credential " + enr.CredentialID + "). It may report only as " +
			in.Hostname + ".",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	a.log.Info("agent enrolled", "hostname", in.Hostname, "credential", enr.CredentialID)

	out := map[string]any{
		"credential":   enr.Credential,
		"credentialId": enr.CredentialID,
		"hostname":     in.Hostname,
	}
	if len(a.Enroll.CAPEM) > 0 {
		out["caPem"] = string(a.Enroll.CAPEM)
	}
	writeJSON(w, http.StatusOK, out)
}
