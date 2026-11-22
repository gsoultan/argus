package control

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

/*
The staff VPN's routes. Everyone signed in sees the staff VPNs and their own
devices, and makes, renews and removes their own; an admin or owner also
sees everyone's devices, sets what each team reaches, the default traffic
and people's own settings, and can remove anyone's device. The hub itself
is a server network, made and approved through /networks/wireguard.
*/

// StaffVPNView is one staff VPN as a person sees it.
type StaffVPNView struct {
	Network WGNetwork `json:"network"`
	// Ready is whether the hub is up to take devices.
	Ready   bool            `json:"ready"`
	Devices []WGStaffDevice `json:"devices"`
	// Grants and Modes are for those who see the whole fleet.
	Grants []WGStaffGrant    `json:"grants,omitempty"`
	Modes  map[string]string `json:"modes,omitempty"`
	Mine   StaffVPNMine      `json:"mine"`
}

// StaffVPNMine is the signed-in person's place on it.
type StaffVPNMine struct {
	Reach  []string `json:"reach"`
	Logins []string `json:"logins"`
	Mode   string   `json:"mode"`
	// NeedsCode is whether making a profile takes an authenticator code,
	// and HasMFA whether the person has one.
	NeedsCode bool `json:"needsCode"`
	HasMFA    bool `json:"hasMfa"`
}

func (a *API) staffErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNoStaffDevice):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrStaffNoReach), errors.Is(err, ErrStaffNotReady):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrStaffCode), errors.Is(err, ErrStaffNoMFA):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		a.wgErr(w, what, err)
	}
}

func (a *API) getStaffVPNs(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx := r.Context()
	all, err := a.store.WGNetworks(ctx)
	if err != nil {
		a.fail(w, "staff vpns", err)
		return
	}
	fleet := seesWholeFleet(sess.Role)
	acct, _ := a.store.Account(ctx, sess.Email)
	out := []StaffVPNView{}
	for _, n := range all {
		if n.Shape != wgShapeStaff {
			continue
		}
		v := StaffVPNView{Network: n}
		if !fleet {
			v.Network.Pending, v.Network.PendingBy = nil, ""
		}
		_, herr := a.store.staffHubOf(ctx, n.ID)
		v.Ready = herr == nil
		who := sess.Email
		if fleet {
			who = ""
			if v.Grants, err = a.store.StaffGrants(ctx, n.ID); err != nil {
				a.fail(w, "staff vpn grants", err)
				return
			}
			if v.Modes, err = a.store.StaffModes(ctx, n.ID); err != nil {
				a.fail(w, "staff vpn modes", err)
				return
			}
		}
		if v.Devices, err = a.store.StaffDevices(ctx, n.ID, who); err != nil {
			a.fail(w, "staff vpn devices", err)
			return
		}
		reach, err := a.store.staffReachAll(ctx, n.ID)
		if err != nil {
			a.fail(w, "staff vpn reach", err)
			return
		}
		def, own, err := a.store.staffModes(ctx, n.ID)
		if err != nil {
			a.fail(w, "staff vpn modes", err)
			return
		}
		v.Mine = StaffVPNMine{Reach: []string{}, Logins: []string{}, Mode: def, NeedsCode: n.Staff != nil && n.Staff.MFA,
			HasMFA: acct != nil && acct.MFAEnrolled}
		if m, ok := own[strings.ToLower(sess.Email)]; ok {
			v.Mine.Mode = m
		}
		if re := reach[strings.ToLower(sess.Email)]; re != nil {
			v.Mine.Reach = uniqSorted(append(append([]string(nil), re.logins...), re.plain...))
			v.Mine.Logins = append(v.Mine.Logins, re.logins...)
		}
		out = append(out, v)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"vpns": out})
}

// staffCode checks the person's authenticator code when the VPN asks for
// one, spending it.
func (a *API) staffCode(r *http.Request, network, email, code string) error {
	h, err := a.store.staffHubOf(r.Context(), network)
	if err != nil && !errors.Is(err, ErrStaffNotReady) {
		return err
	}
	if !h.policy.MFA {
		return nil
	}
	acct, err := a.store.Account(r.Context(), email)
	if err != nil || acct == nil {
		return ErrStaffCode
	}
	if !acct.MFAEnrolled {
		return ErrStaffNoMFA
	}
	ok, err := a.store.ConsumeTOTP(r.Context(), acct, strings.TrimSpace(code), time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrStaffCode
	}
	return nil
}

func (a *API) postStaffDevice(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if err := a.staffCode(r, id, sess.Email, in.Code); err != nil {
		a.staffErr(w, "staff device", err)
		return
	}
	p, err := a.store.MakeStaffDevice(r.Context(), id, sess.Email, in.Name)
	if err != nil {
		a.staffErr(w, "staff device", err)
		return
	}
	a.audit(r, "staff_vpn.device.made", sess.Email, "staff vpn device "+p.Device.Name,
		"Made a staff VPN profile for "+p.Device.Name+" at "+p.Device.Address+", "+p.Mode+" traffic, lasting until "+
			p.Device.ExpiresAt.UTC().Format("2 Jan 2006")+". The private key was handed over once and not kept.")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, p)
}

// ownDevice reads a device the session may act on: its own, or any for an
// admin or owner.
func (a *API) ownDevice(w http.ResponseWriter, r *http.Request) (WGStaffDevice, string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return WGStaffDevice{}, "", false
	}
	d, err := a.store.staffDevice(r.Context(), r.PathValue("id"))
	admin := sess.Role == "admin" || sess.Role == "owner"
	if err != nil || (!admin && !strings.EqualFold(d.UserEmail, sess.Email)) {
		writeErr(w, http.StatusNotFound, ErrNoStaffDevice.Error())
		return WGStaffDevice{}, "", false
	}
	return d, sess.Email, true
}

func (a *API) postStaffRenew(w http.ResponseWriter, r *http.Request) {
	d, actor, ok := a.ownDevice(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(d.UserEmail, actor) {
		writeErr(w, http.StatusForbidden, "only the device's person renews it, with their own code")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	_ = decode(r, &in)
	if err := a.staffCode(r, d.NetworkID, actor, in.Code); err != nil {
		a.staffErr(w, "renew staff device", err)
		return
	}
	d, err := a.store.RenewStaffDevice(r.Context(), d.ID)
	if err != nil {
		a.staffErr(w, "renew staff device", err)
		return
	}
	a.audit(r, "staff_vpn.device.renewed", actor, "staff vpn device "+d.Name,
		"Renewed the staff VPN profile "+d.Name+" until "+d.ExpiresAt.UTC().Format("2 Jan 2006")+".")
	writeJSON(w, http.StatusOK, d)
}

func (a *API) deleteStaffDevice(w http.ResponseWriter, r *http.Request) {
	d, actor, ok := a.ownDevice(w, r)
	if !ok {
		return
	}
	if err := a.store.RemoveStaffDevice(r.Context(), d.ID); err != nil {
		a.staffErr(w, "remove staff device", err)
		return
	}
	a.audit(r, "staff_vpn.device.removed", actor, "staff vpn device "+d.Name,
		"Removed the staff VPN profile "+d.Name+" ("+d.UserEmail+", "+d.Address+"). The VPN's server drops it within a minute.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putStaffPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	var p WGStaffPolicy
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkStaffPolicy(&p); err != nil {
		a.staffErr(w, "staff vpn policy", err)
		return
	}
	if err := a.store.SetStaffPolicy(r.Context(), r.PathValue("id"), p); err != nil {
		a.staffErr(w, "staff vpn policy", err)
		return
	}
	code := "no authenticator code"
	if p.MFA {
		code = "an authenticator code"
	}
	a.audit(r, "staff_vpn.policy", actor, "staff vpn "+r.PathValue("id"),
		"Set the staff VPN to "+p.Default+" traffic by default, profiles lasting "+itoa(p.Days)+" days, made and renewed with "+code+".")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putStaffGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Ranges []string `json:"ranges"`
		Logins bool     `json:"logins"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ranges, err := checkStaffRanges(in.Ranges)
	if err != nil {
		a.staffErr(w, "staff vpn grant", err)
		return
	}
	team := r.PathValue("team")
	if err := a.store.SetStaffGrant(r.Context(), r.PathValue("id"), team, ranges, in.Logins, actor); err != nil {
		a.staffErr(w, "staff vpn grant", err)
		return
	}
	what := "Took away the team's reach on the staff VPN."
	if len(ranges) > 0 {
		what = "The team reaches " + strings.Join(ranges, ", ") + " on the staff VPN, with SSH and RDP straight to them "
		if in.Logins {
			what += "allowed. Those logins are not recorded by Argus."
		} else {
			what += "refused, so logins go through Argus and are recorded."
		}
	}
	a.audit(r, "staff_vpn.grant", actor, "team "+team, what)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putStaffMode(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.wgAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Email string `json:"email"`
		Mode  string `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.SetStaffMode(r.Context(), r.PathValue("id"), in.Email, in.Mode, actor); err != nil {
		a.staffErr(w, "staff vpn mode", err)
		return
	}
	mode := in.Mode
	if mode == "" {
		mode = "the default"
	}
	a.audit(r, "staff_vpn.mode", actor, in.Email, in.Email+"'s staff VPN traffic is "+mode+".")
	w.WriteHeader(http.StatusNoContent)
}
