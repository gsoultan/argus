package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

/*
The console's routes for WireGuard networks (wg_networks.go).

	GET    /api/v1/networks/wireguard              the networks (admin, owner, auditor)
	POST   /api/v1/networks/wireguard              ask for a new one (admin, owner)
	PUT    /api/v1/networks/wireguard/{id}         ask for a change
	DELETE /api/v1/networks/wireguard/{id}         ask for it to be removed
	POST   /api/v1/networks/wireguard/{id}/approve approve the waiting change
	POST   /api/v1/networks/wireguard/{id}/decline decline it, or take back your own
*/

func wgSubject(id string) string { return "wg-network:" + id }

func (a *API) wgAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can set up WireGuard networks")
		return "", false
	}
	return sess.Email, true
}

func (a *API) wgErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidWGNetwork
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.msg)
	case errors.Is(err, ErrNoWGNetwork):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrWGPending), errors.Is(err, ErrWGNoPending), errors.Is(err, ErrWGNameExists):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.fail(w, what, err)
	}
}

type wgNetworksView struct {
	Networks []WGNetwork `json:"networks"`
}

func (a *API) getWGNetworks(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing the networks requires the admin, owner or auditor role")
		return
	}
	list, err := a.store.WGNetworks(r.Context())
	if err != nil {
		a.fail(w, "wireguard networks", err)
		return
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
	writeJSON(w, http.StatusOK, wgNetworksView{Networks: list})
}

// readWGDraft reads and checks a draft for network id ("" for a new one).
func (a *API) readWGDraft(r *http.Request, id string) (WGNetworkDraft, error) {
	var d WGNetworkDraft
	if err := decode(r, &d); err != nil {
		return d, ErrInvalidWGNetwork{err.Error()}
	}
	d.Remove = false
	hosts, err := a.store.wgHosts(r.Context())
	if err != nil {
		return d, err
	}
	taken, err := a.store.wgTakenFor(r.Context(), id)
	if err != nil {
		return d, err
	}
	return d, checkWGDraft(&d, hosts, taken)
}

func (a *API) askWGApproval(r *http.Request, actor string, n WGNetwork, what string) {
	a.raiseAlert(r.Context(), ConsoleAlert{
		Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: actor,
		Title:   actor + " asks to " + what,
		Detail:  "Nothing changes on the servers until another admin approves it on the Networks page.",
		Link:    "/networks",
		Subject: wgSubject(n.ID),
	})
}

func wgDraftSummary(d WGNetworkDraft) string {
	hosts := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		h := m.Hostname + " " + m.Address
		if m.Hub {
			h += " (hub)"
		}
		if len(m.Routes) > 0 {
			h += " routing " + strings.Join(m.Routes, ", ")
		}
		hosts = append(hosts, h)
	}
	return fmt.Sprintf("%s network %s on UDP %d: %s", d.Shape, d.CIDR, d.Port, strings.Join(hosts, ", "))
}

func (a *API) postWGNetwork(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	d, err := a.readWGDraft(r, "")
	if err != nil {
		a.wgErr(w, "add wireguard network", err)
		return
	}
	n, err := a.store.CreateWGNetwork(r.Context(), d, actor)
	if err != nil {
		a.wgErr(w, "add wireguard network", err)
		return
	}
	a.audit(r, "wireguard.network.request", actor, "wireguard network "+n.Name,
		"Asked for the WireGuard network "+n.Name+": "+wgDraftSummary(d)+". It waits for another admin to approve it.")
	a.askWGApproval(r, actor, n, "set up the WireGuard network "+n.Name)
	writeJSON(w, http.StatusCreated, n)
}

func (a *API) putWGNetwork(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, err := a.store.wgNetwork(r.Context(), id)
	if err != nil {
		a.wgErr(w, "change wireguard network", err)
		return
	}
	if before.State == "waiting" {
		writeErr(w, http.StatusConflict, "this network has not been approved yet: decline it and ask again")
		return
	}
	d, err := a.readWGDraft(r, id)
	if err != nil {
		a.wgErr(w, "change wireguard network", err)
		return
	}
	n, err := a.store.ProposeWGChange(r.Context(), id, d, actor)
	if err != nil {
		a.wgErr(w, "change wireguard network", err)
		return
	}
	a.audit(r, "wireguard.network.request", actor, "wireguard network "+before.Name,
		"Asked to change the WireGuard network "+before.Name+" to: "+wgDraftSummary(d)+". It waits for another admin to approve it.")
	a.askWGApproval(r, actor, n, "change the WireGuard network "+before.Name)
	writeJSON(w, http.StatusOK, n)
}

func (a *API) deleteWGNetwork(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, err := a.store.wgNetwork(r.Context(), id)
	if err != nil {
		a.wgErr(w, "remove wireguard network", err)
		return
	}
	if before.State == "waiting" {
		writeErr(w, http.StatusConflict, "this network is on no server yet: decline it instead")
		return
	}
	n, err := a.store.ProposeWGChange(r.Context(), id, WGNetworkDraft{Name: before.Name, Shape: before.Shape,
		CIDR: before.CIDR, Port: before.Port, Remove: true}, actor)
	if err != nil {
		a.wgErr(w, "remove wireguard network", err)
		return
	}
	a.audit(r, "wireguard.network.request", actor, "wireguard network "+before.Name,
		"Asked to remove the WireGuard network "+before.Name+" from its servers. It waits for another admin to approve it.")
	a.askWGApproval(r, actor, n, "remove the WireGuard network "+before.Name)
	writeJSON(w, http.StatusAccepted, n)
}

func (a *API) postWGDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.wgAdmin(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		cur, err := a.store.wgNetwork(r.Context(), id)
		if err != nil {
			a.wgErr(w, "decide wireguard network", err)
			return
		}
		if cur.Pending == nil {
			a.wgErr(w, "decide wireguard network", ErrWGNoPending)
			return
		}
		// Declining your own is taking it back; approving it needs someone
		// else, unless nobody else could (self_approval.go).
		if approve && strings.EqualFold(cur.PendingBy, actor) {
			confirm, err := readConfirmSelf(r)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !a.approveOwn(w, r, actor, true, runDeciders, false, confirm,
				"another admin approves a change to a WireGuard network", "WireGuard network change", cur.Name) {
				return
			}
		}
		if approve {
			// Checked again: what it would clash with may have changed
			// since it was asked for.
			if !cur.Pending.Remove {
				d := *cur.Pending
				hosts, err := a.store.wgHosts(r.Context())
				if err != nil {
					a.fail(w, "approve wireguard network", err)
					return
				}
				taken, err := a.store.wgTakenFor(r.Context(), id)
				if err != nil {
					a.fail(w, "approve wireguard network", err)
					return
				}
				if err := checkWGDraft(&d, hosts, taken); err != nil {
					a.wgErr(w, "approve wireguard network", err)
					return
				}
			}
			before, removed, err := a.store.ApproveWGNetwork(r.Context(), id, actor)
			if err != nil {
				a.wgErr(w, "approve wireguard network", err)
				return
			}
			detail := "Approved " + before.PendingBy + "'s change to the WireGuard network " + before.Name + ": " +
				wgDraftSummary(*before.Pending) + ". The servers apply it on their next heartbeat."
			if removed {
				detail = "Approved removing the WireGuard network " + before.Name + ". Its servers take it down on their next heartbeat."
			}
			a.audit(r, "wireguard.network.approve", actor, "wireguard network "+before.Name, detail)
			a.closeAlerts(r.Context(), wgSubject(id), "Approved by "+actor)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		before, deleted, err := a.store.DeclineWGNetwork(r.Context(), id)
		if err != nil {
			a.wgErr(w, "decline wireguard network", err)
			return
		}
		verb := "Declined"
		if strings.EqualFold(before.PendingBy, actor) {
			verb = "Took back"
		}
		detail := fmt.Sprintf("%s %s's change to the WireGuard network %s.", verb, before.PendingBy, before.Name)
		if deleted {
			detail += " The network was never approved, so it is gone."
		}
		a.audit(r, "wireguard.network.decline", actor, "wireguard network "+before.Name, detail)
		a.closeAlerts(r.Context(), wgSubject(id), verb+" by "+actor)
		w.WriteHeader(http.StatusNoContent)
	}
}
