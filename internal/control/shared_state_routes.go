package control

import (
	"net/http"
	"time"
)

// postRedeem burns a terminal ticket, shared across every gateway.
//
// Reporter-authenticated: only a gateway calls this, and a browser must never
// be able to burn someone else's ticket as a denial of service.
func (a *API) postRedeem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		Target    string    `json:"target"`
		Principal string    `json:"principal"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ID == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = time.Now().Add(5 * time.Minute)
	}

	used, err := a.store.RedeemTicket(r.Context(), in.ID, in.Email, in.Target,
		in.Principal, in.ExpiresAt)
	if err != nil {
		a.fail(w, "redeem ticket", err)
		return
	}
	if used {
		a.log.Warn("ticket replay refused",
			"ticket", in.ID, "email", in.Email, "target", in.Target)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"alreadyUsed": used})
}

// getHostKeyPin returns a target's pin so every gateway verifies against the
// same recorded identity.
func (a *API) getHostKeyPin(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	pin, err := a.store.HostKeyPin(r.Context(), host)
	if err != nil {
		a.fail(w, "host key pin", err)
		return
	}
	if pin == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pinned": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": true, "pin": pin})
}

// postHostKeyPin records a first-contact pin.
func (a *API) postHostKeyPin(w http.ResponseWriter, r *http.Request) {
	var in HostKeyPin
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Host == "" || in.Fingerprint == "" {
		writeErr(w, http.StatusBadRequest, "host and fingerprint are required")
		return
	}
	if in.PinnedBy == "" {
		in.PinnedBy = "tofu"
	}

	conflicting, err := a.store.PinHostKey(r.Context(), in)
	if err != nil {
		a.fail(w, "pin host key", err)
		return
	}
	if conflicting != nil {
		// Another gateway already pinned a different key for this host. That is
		// either a rebuild nobody recorded or an interception, and it is not
		// something to resolve by overwriting.
		a.log.Error("host key pin conflict",
			"host", in.Host,
			"existing", conflicting.Fingerprint,
			"presented", in.Fingerprint,
			"detail", "refusing to overwrite; verify out of band and repin explicitly")
		if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:   "hostkey.mismatch",
			Severity: "critical",
			Target:   in.Host,
			Detail: "A gateway presented " + in.Fingerprint + " but " +
				conflicting.Fingerprint + " is pinned. The pin was not changed.",
		}); aerr != nil {
			a.log.Error("audit append failed", "error", aerr)
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "host key does not match the pinned fingerprint",
			"pin":   conflicting,
		})
		return
	}

	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "hostkey.pin",
		Severity:   "notice",
		ActorEmail: in.PinnedBy,
		Target:     in.Host,
		Detail:     "Pinned " + in.Fingerprint + ".",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pinned"})
}
