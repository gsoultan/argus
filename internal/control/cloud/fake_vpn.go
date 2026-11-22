package cloud

import (
	"context"
	"errors"
	"fmt"
)

// CreateVPN adds the connection to the inventory as pending, with two
// tunnel addresses, as AWS would. Like AWS it refuses a VPC it does not
// have and a call without a key.
func (f *Fake) CreateVPN(_ context.Context, a Account, actor Actor, s VPNSpec) (VPNMade, error) {
	if err := f.storageCall(a, ActionVPNCreate, s.Network+" "+s.CustomerAddress, actor); err != nil {
		return VPNMade{}, err
	}
	if err := s.Check(); err != nil {
		return VPNMade{}, err
	}
	if s.PSK == "" {
		return VPNMade{}, errors.New("InvalidParameterValue: no pre-shared key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var vpc *Resource
	gw := ""
	for i, r := range f.Resources[s.Region] {
		if r.Kind == KindNetwork && r.ID == s.Network {
			vpc = &f.Resources[s.Region][i]
		}
	}
	if vpc == nil {
		return VPNMade{}, errors.New("InvalidVpcID.NotFound: " + s.Network)
	}
	made := VPNMade{}
	if gw = vpc.Details["vpnGateway"]; gw == "" {
		f.made++
		gw = fmt.Sprintf("vgw-0000fa4e%04d", f.made)
		if vpc.Details == nil {
			vpc.Details = map[string]string{}
		}
		vpc.Details["vpnGateway"] = gw
		made.NewGateway = true
	}
	f.made++
	id := fmt.Sprintf("vpn-0000fa4e%04d", f.made)
	made.Tunnels = []string{fmt.Sprintf("203.0.113.%d", 10+f.made%200), fmt.Sprintf("203.0.113.%d", 11+f.made%200)}
	tags := map[string]string{"Name": s.Name, VPNLinkTag: s.Name}
	for k, v := range s.Tags {
		tags[k] = v
	}
	f.Resources[s.Region] = append(f.Resources[s.Region], Resource{Kind: KindVPN, ID: id, Name: s.Name, Region: s.Region,
		State: "pending", Tags: tags, Details: map[string]string{DetailGateway: gw, DetailVPNType: "ipsec.1",
			DetailRemoteAddress: s.CustomerAddress, DetailRemoteNetworks: s.ServerRange, DetailRouting: "static"}})
	made.Result = Result{ResourceID: id, RequestID: f.request()}
	made.Gateway, made.Propagated = gw, s.Propagate && !made.NewGateway
	return made, nil
}
