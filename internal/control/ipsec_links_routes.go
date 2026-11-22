package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The console's routes for IPsec links (ipsec_links.go).

	GET    /api/v1/networks/ipsec              the links (admin, owner, auditor)
	POST   /api/v1/networks/ipsec              ask for a new one (admin, owner)
	PUT    /api/v1/networks/ipsec/{id}         ask for a change
	DELETE /api/v1/networks/ipsec/{id}         ask for it to be removed
	POST   /api/v1/networks/ipsec/{id}/approve approve the waiting change
	POST   /api/v1/networks/ipsec/{id}/decline decline it, or take back your own
	POST   /api/v1/networks/ipsec/{id}/key     the pre-shared key, to enter in the cloud (audited)
*/

func ipsecSubject(id string) string { return "ipsec-link:" + id }

func (a *API) ipsecErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNoIPsecLink):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrIPsecNameExists):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.wgErr(w, what, err)
	}
}

func (a *API) getIPsecLinks(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing the networks requires the admin, owner or auditor role")
		return
	}
	list, err := a.store.IPsecLinks(r.Context())
	if err != nil {
		a.fail(w, "ipsec links", err)
		return
	}
	if vpns, err := a.store.CloudResources(r.Context(), cloud.KindVPN); err == nil {
		matchCloudEnds(list, vpns)
	}
	sole, known := false, false
	for i := range list {
		if list[i].Pending == nil || !strings.EqualFold(list[i].PendingBy, sess.Email) {
			continue
		}
		if !known {
			if sole, err = a.store.SoleDecider(r.Context(), sess.Email, runDeciders); err != nil {
				a.fail(w, "self approval", err)
				return
			}
			known = true
		}
		list[i].ApproveOwn = sole
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": list})
}

// readIPsecInput reads and checks a draft, sealing a key it brings or makes.
// isNew makes a key when none is given.
func (a *API) readIPsecInput(r *http.Request, isNew bool) (IPsecDraft, []byte, error) {
	var in IPsecInput
	if err := decode(r, &in); err != nil {
		return IPsecDraft{}, nil, ErrInvalidWGNetwork{err.Error()}
	}
	d := in.IPsecDraft
	d.Remove, d.PSKSealed = false, nil
	hosts, err := a.store.wgHosts(r.Context())
	if err != nil {
		return d, nil, err
	}
	taken, err := a.store.wgTakenFor(r.Context(), "")
	if err != nil {
		return d, nil, err
	}
	if err := checkIPsecDraft(&d, hosts, taken.own); err != nil {
		return d, nil, err
	}
	psk := strings.TrimSpace(in.PSK)
	if psk == "" && !isNew && !d.NewKey {
		return d, nil, nil
	}
	if psk == "" {
		if psk, err = newPSK(); err != nil {
			return d, nil, err
		}
	} else if !reIPsecPSK.MatchString(psk) {
		return d, nil, invalidWG("the pre-shared key is 16 to 64 letters, digits, dots and underscores")
	}
	sealed, err := a.sealPSK(r.Context(), psk)
	if err != nil {
		return d, nil, err
	}
	d.NewKey = true
	return d, sealed, nil
}

func ipsecWords(d IPsecDraft) string {
	key := ""
	if d.NewKey {
		key = ", with a new pre-shared key"
	}
	return fmt.Sprintf("%s to %s, %s ⇄ %s, %s ciphers%s", d.Hostname, d.Peer, strings.Join(d.LocalRanges, ", "),
		strings.Join(d.RemoteRanges, ", "), d.Proposals, key)
}

func (a *API) askIPsecApproval(r *http.Request, actor, id, what string) {
	a.raiseAlert(r.Context(), ConsoleAlert{
		Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: actor,
		Title:   actor + " asks to " + what,
		Detail:  "Nothing changes on the server until another admin approves it on the Networks page.",
		Link:    "/networks",
		Subject: ipsecSubject(id),
	})
}

func (a *API) postIPsecLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	d, sealed, err := a.readIPsecInput(r, true)
	if err != nil {
		a.ipsecErr(w, "add ipsec link", err)
		return
	}
	l, err := a.store.CreateIPsecLink(r.Context(), d, sealed, actor)
	if err != nil {
		a.ipsecErr(w, "add ipsec link", err)
		return
	}
	a.audit(r, "ipsec.link.request", actor, "ipsec link "+l.Name,
		"Asked for the IPsec link "+l.Name+": "+ipsecWords(d)+". It waits for another admin to approve it.")
	a.askIPsecApproval(r, actor, l.ID, "set up the IPsec link "+l.Name)
	writeJSON(w, http.StatusCreated, l)
}

func (a *API) putIPsecLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, _, err := a.store.ipsecLink(r.Context(), id)
	if err != nil {
		a.ipsecErr(w, "change ipsec link", err)
		return
	}
	if before.State == "waiting" {
		writeErr(w, http.StatusConflict, "this link has not been approved yet: decline it and ask again")
		return
	}
	d, sealed, err := a.readIPsecInput(r, false)
	if err != nil {
		a.ipsecErr(w, "change ipsec link", err)
		return
	}
	d.PSKSealed = sealed
	if err := a.store.ProposeIPsecChange(r.Context(), id, d, actor); err != nil {
		a.ipsecErr(w, "change ipsec link", err)
		return
	}
	a.audit(r, "ipsec.link.request", actor, "ipsec link "+before.Name,
		"Asked to change the IPsec link "+before.Name+" to: "+ipsecWords(d)+". It waits for another admin to approve it.")
	a.askIPsecApproval(r, actor, id, "change the IPsec link "+before.Name)
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) deleteIPsecLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, _, err := a.store.ipsecLink(r.Context(), id)
	if err != nil {
		a.ipsecErr(w, "remove ipsec link", err)
		return
	}
	if before.State == "waiting" {
		writeErr(w, http.StatusConflict, "this link is on no server yet: decline it instead")
		return
	}
	d := IPsecDraft{Name: before.Name, AssetID: before.AssetID, Hostname: before.Hostname, Peer: before.Peer,
		LocalRanges: before.LocalRanges, RemoteRanges: before.RemoteRanges, Proposals: before.Proposals, Remove: true}
	if err := a.store.ProposeIPsecChange(r.Context(), id, d, actor); err != nil {
		a.ipsecErr(w, "remove ipsec link", err)
		return
	}
	a.audit(r, "ipsec.link.request", actor, "ipsec link "+before.Name,
		"Asked to remove the IPsec link "+before.Name+" from "+before.Hostname+". It waits for another admin to approve it.")
	a.askIPsecApproval(r, actor, id, "remove the IPsec link "+before.Name)
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) postIPsecDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.wgAdmin(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		cur, _, err := a.store.ipsecLink(r.Context(), id)
		if err != nil {
			a.ipsecErr(w, "decide ipsec link", err)
			return
		}
		if cur.Pending == nil {
			a.ipsecErr(w, "decide ipsec link", ErrWGNoPending)
			return
		}
		if approve && strings.EqualFold(cur.PendingBy, actor) {
			confirm, err := readConfirmSelf(r)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !a.approveOwn(w, r, actor, true, runDeciders, false, confirm,
				"another admin approves a change to an IPsec link", "IPsec link change", cur.Name) {
				return
			}
		}
		if approve {
			if !cur.Pending.Remove {
				d := *cur.Pending
				hosts, err := a.store.wgHosts(r.Context())
				if err != nil {
					a.fail(w, "approve ipsec link", err)
					return
				}
				taken, err := a.store.wgTakenFor(r.Context(), "")
				if err != nil {
					a.fail(w, "approve ipsec link", err)
					return
				}
				if err := checkIPsecDraft(&d, hosts, taken.own); err != nil {
					a.ipsecErr(w, "approve ipsec link", err)
					return
				}
			}
			before, d, removed, err := a.store.ApproveIPsecLink(r.Context(), id, actor)
			if err != nil {
				a.ipsecErr(w, "approve ipsec link", err)
				return
			}
			detail := "Approved " + before.PendingBy + "'s IPsec link " + before.Name + ": " + ipsecWords(*d) +
				". The server applies it on its next heartbeat."
			if removed {
				detail = "Approved removing the IPsec link " + before.Name + ". " + before.Hostname + " takes it down on its next heartbeat."
			}
			a.audit(r, "ipsec.link.approve", actor, "ipsec link "+before.Name, detail)
			a.closeAlerts(r.Context(), ipsecSubject(id), "Approved by "+actor)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		before, deleted, err := a.store.DeclineIPsecLink(r.Context(), id)
		if err != nil {
			a.ipsecErr(w, "decline ipsec link", err)
			return
		}
		verb := "Declined"
		if strings.EqualFold(before.PendingBy, actor) {
			verb = "Took back"
		}
		detail := fmt.Sprintf("%s %s's change to the IPsec link %s.", verb, before.PendingBy, before.Name)
		if deleted {
			detail += " The link was never approved, so it is gone."
		}
		a.audit(r, "ipsec.link.decline", actor, "ipsec link "+before.Name, detail)
		a.closeAlerts(r.Context(), ipsecSubject(id), verb+" by "+actor)
		w.WriteHeader(http.StatusNoContent)
	}
}

// postIPsecKey shows an admin the link's key, to enter in the cloud or on
// the office firewall. Every look is audited.
func (a *API) postIPsecKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	l, sealed, err := a.store.ipsecLink(r.Context(), r.PathValue("id"))
	if err != nil {
		a.ipsecErr(w, "ipsec key", err)
		return
	}
	psk, err := a.openPSK(r.Context(), sealed)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "ipsec.link.key_shown", Severity: "notice", ActorEmail: actor, Target: "ipsec link " + l.Name,
		Detail: "Was shown the pre-shared key of the IPsec link " + l.Name + " to " + l.Peer + ".",
	}); err != nil {
		a.fail(w, "ipsec key", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"psk": psk})
}
