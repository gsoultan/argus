package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Making the cloud end of an IPsec link (AWS first). Asking is a Change-level
cloud change with the usual plan and approval. Applied, it makes the
Site-to-Site VPN connection with a key Argus makes on the spot, then asks
for the IPsec link from the chosen server to the connection's first
tunnel, with that key sealed. The link waits for its own approval, like any
change to a server. The key is never in the change.
*/

// cloudVPNSpec is what making a cloud VPN asks for, as stored.
type cloudVPNSpec struct {
	Name          string `json:"name"`
	Network       string `json:"network"`
	NetworkName   string `json:"networkName,omitempty"`
	AssetID       string `json:"assetId"`
	Hostname      string `json:"hostname,omitempty"`
	ServerAddress string `json:"serverAddress"`
	ServerRange   string `json:"serverRange"`
	CloudRange    string `json:"cloudRange"`
	Ciphers       string `json:"ciphers"`
	Propagate     bool   `json:"propagate"`
}

// vpnHourly is AWS's list price for a Site-to-Site VPN connection in most
// regions, for the plan's words.
const vpnHourly = 0.05

// draftForVPN is the IPsec link a cloud VPN asks for, to peer.
func draftForVPN(s cloudVPNSpec, peer string) IPsecDraft {
	return IPsecDraft{Name: s.Name, AssetID: s.AssetID, Peer: peer, LocalRanges: []string{s.ServerRange},
		RemoteRanges: []string{s.CloudRange}, Proposals: s.Ciphers}
}

func (a *API) planVPN(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.VPNs); !ok || m.acct.Provider != "aws" {
		return ch, http.StatusNotImplemented, errors.New("Argus makes cloud VPN connections in AWS so far; make it in the provider's console and add the link with its address")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ctx := r.Context()
	var s cloudVPNSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a cloud VPN takes {name, network, assetId, serverAddress, serverRange, cloudRange, ciphers}")
	}
	s.Network, s.ServerAddress = strings.TrimSpace(s.Network), strings.TrimSpace(s.ServerAddress)
	s.ServerRange, s.CloudRange = strings.TrimSpace(s.ServerRange), strings.TrimSpace(s.CloudRange)
	vpc, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindNetwork, s.Network)
	if err != nil {
		return ch, http.StatusNotFound, errors.New("Argus has not read that VPC in " + in.Region)
	}
	s.NetworkName = vpc.Name
	if s.CloudRange == "" {
		s.CloudRange = vpc.Size
	}
	if p, err := netip.ParsePrefix(s.CloudRange); err == nil {
		if v, err := netip.ParsePrefix(vpc.Size); err == nil && (p.Bits() < v.Bits() || !v.Contains(p.Addr())) {
			return ch, http.StatusBadRequest, fmt.Errorf("%s is not inside %s's range %s", s.CloudRange, vpc.Name, vpc.Size)
		}
	}
	// The link it will ask for must pass the link's own checks; its peer
	// is not known until AWS gives the tunnels their addresses.
	d := draftForVPN(s, "192.0.2.1")
	hosts, err := a.store.wgHosts(ctx)
	if err != nil {
		return ch, http.StatusInternalServerError, err
	}
	taken, err := a.store.wgTakenFor(ctx, "")
	if err != nil {
		return ch, http.StatusInternalServerError, err
	}
	if err := checkIPsecDraft(&d, hosts, taken.own); err != nil {
		return ch, http.StatusBadRequest, err
	}
	s.Name, s.Hostname, s.Ciphers = d.Name, d.Hostname, d.Proposals
	s.ServerRange, s.CloudRange = d.LocalRanges[0], d.RemoteRanges[0]
	var exists bool
	if err := a.store.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ipsec_links WHERE name = $1)`, s.Name).Scan(&exists); err != nil {
		return ch, http.StatusInternalServerError, err
	}
	if exists {
		return ch, http.StatusConflict, ErrIPsecNameExists
	}
	if err := (cloud.VPNSpec{Region: in.Region, Name: s.Name, Network: s.Network, CustomerAddress: s.ServerAddress,
		CloudRange: s.CloudRange, ServerRange: s.ServerRange, Ciphers: s.Ciphers}).Check(); err != nil {
		return ch, http.StatusBadRequest, err
	}
	ch.Spec, _ = json.Marshal(s)
	ch.ResourceName = s.Name
	ch.Environment = cloudEnvOf(vpc.Tags, m.acct.Environment)
	ch.Summary = vpnWords(s)
	return ch, http.StatusOK, nil
}

// vpnWords is a cloud VPN's plan in a few sentences, with its price.
func vpnWords(s cloudVPNSpec) string {
	w := fmt.Sprintf("Make the AWS Site-to-Site VPN %q from VPC %s (%s) to %s at %s (%s), %s ciphers, static routing. "+
		"It uses the VPC's virtual private gateway, or makes and attaches one when it has none",
		s.Name, s.NetworkName, s.CloudRange, s.Hostname, s.ServerAddress, s.ServerRange, s.Ciphers)
	if s.Propagate {
		w += ", and the VPC's route tables learn " + s.ServerRange + " through it"
	}
	w += fmt.Sprintf(". AWS bills from about US$%.2f an hour per connection in most regions (about $%.0f a month), plus data sent out of AWS. "+
		"Then Argus asks for the IPsec link on %s to the first tunnel, with a key it makes; that link waits for its own approval",
		vpnHourly, vpnHourly*730, s.Hostname)
	return w
}

func (a *API) applyVPN(ctx context.Context, r *http.Request, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	vp, ok := m.manager.(cloud.VPNs)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot make VPN connections from Argus yet")
	}
	var s cloudVPNSpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	psk, err := newPSK()
	if err != nil {
		return cloud.Result{}, err
	}
	sealed, err := a.sealPSK(ctx, psk)
	if err != nil {
		return cloud.Result{}, err
	}
	made, err := vp.CreateVPN(ctx, m.target, cloud.Actor(ch.RequestedBy), cloud.VPNSpec{Region: ch.Region, Name: s.Name,
		Network: s.Network, CustomerAddress: s.ServerAddress, CloudRange: s.CloudRange, ServerRange: s.ServerRange,
		Ciphers: s.Ciphers, PSK: psk, Propagate: s.Propagate,
		Tags: map[string]string{"argus:change": ch.ID, "argus:launched-by": ch.RequestedBy}})
	if err != nil {
		return made.Result, err
	}
	res := made.Result
	words := []string{fmt.Sprintf("AWS made %s", res.ResourceID)}
	if made.NewGateway {
		words = append(words, fmt.Sprintf("with virtual private gateway %s, attached to %s", made.Gateway, s.NetworkName))
	}
	if s.Propagate && !made.Propagated {
		words = append(words, fmt.Sprintf("Turn on route propagation from %s in %s's route tables once it is attached, so replies find their way back", made.Gateway, s.NetworkName))
	}
	if len(made.Tunnels) == 0 {
		words = append(words, "AWS gave no tunnel addresses yet, so no IPsec link was asked for; add it from Networks once the connection shows its tunnels")
		res.Detail = strings.Join(words, ". ")
		return res, nil
	}
	d := draftForVPN(s, made.Tunnels[0])
	hosts, err := a.store.wgHosts(ctx)
	if err == nil {
		if taken, terr := a.store.wgTakenFor(ctx, ""); terr != nil {
			err = terr
		} else {
			err = checkIPsecDraft(&d, hosts, taken.own)
		}
	}
	var l IPsecLink
	if err == nil {
		d.NewKey = true
		l, err = a.store.CreateIPsecLink(ctx, d, sealed, ch.RequestedBy)
	}
	if err != nil {
		a.log.Error("ipsec link for a cloud vpn", "change", ch.ID, "error", err)
		words = append(words, "The IPsec link could not be asked for ("+err.Error()+"); add it from Networks to "+made.Tunnels[0]+
			", and set the same key on both tunnels in AWS")
		res.Detail = strings.Join(words, ". ")
		return res, nil
	}
	a.audit(r, "ipsec.link.request", ch.RequestedBy, "ipsec link "+l.Name,
		"Asked for the IPsec link "+l.Name+" to AWS connection "+res.ResourceID+": "+ipsecWords(d)+". It waits for another admin to approve it.")
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: ch.RequestedBy,
		Title:   ch.RequestedBy + " asks to set up the IPsec link " + l.Name,
		Detail:  "AWS made the connection. Nothing changes on " + d.Hostname + " until another admin approves the link on the Networks page.",
		Link:    "/networks",
		Subject: ipsecSubject(l.ID)})
	tunnels := made.Tunnels[0]
	if len(made.Tunnels) > 1 {
		tunnels += "; AWS's second tunnel, " + made.Tunnels[1] + ", stays unused"
	}
	words = append(words, fmt.Sprintf("Argus asked for the IPsec link %s on %s to %s. It waits for another admin's approval on Networks",
		l.Name, d.Hostname, tunnels))
	res.Detail = strings.Join(words, ". ")
	return res, nil
}
