package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Public IP addresses. An address is known to Argus by its resourceGuid and
found by listing the subscription's, as VMs and disks are.

An address is not attached to a VM but to an IP configuration of one of its
network interfaces, and Azure has no call that sets only that: the whole
interface is written back. So Argus reads the interface, changes the one
reference to the address and nothing else, and writes it with If-Match on
the etag it read: when someone else changed the interface in between,
Azure refuses and nothing is overwritten.

Argus makes Standard, static, IPv4 addresses: what Azure makes by default
since Basic addresses were retired.
*/

var reNICName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,78}[A-Za-z0-9_])?$`)

// ValidAddressName reports whether n can name a public IP address: the
// same rule as a disk's.
func ValidAddressName(n string) bool { return ValidDiskName(n) }

// AddressRef is how a new address is named to AllocateAddress: Azure keeps
// an address in a resource group, which the other clouds have none of.
func AddressRef(group, name string) string { return group + "/" + name }

type publicIP struct {
	Name, Path, Group string
	IP                string
	Tags              map[string]string
	// IPConfig is the path of the IP configuration that has it, as Azure
	// gave it; empty when nothing does. Gateway is set when a NAT gateway
	// has it.
	IPConfig string
	Gateway  bool
}

func addressPath(subscription, rg, name string) string {
	return SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Network/publicIPAddresses/" + url.PathEscape(name)
}

// locateAddress finds the address with resourceGuid id in region.
func (p *Provider) locateAddress(ctx context.Context, t token, actor cloud.Actor, subscription, region, id string) (publicIP, error) {
	if !ValidGUID(id) {
		return publicIP{}, fmt.Errorf("%q is not an address id", id)
	}
	var found *raw
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/publicIPAddresses?api-version="+networkAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for i := range items {
				if strings.EqualFold(items[i].Properties.ResourceGUID, id) {
					found = &items[i]
				}
			}
			return nil
		})
	if err != nil {
		return publicIP{}, err
	}
	if found == nil {
		return publicIP{}, fmt.Errorf("subscription %s has no public address with id %s", subscription, id)
	}
	if got := place(found.Location); got != region {
		return publicIP{}, fmt.Errorf("%s is in %s, not %s", found.Name, got, region)
	}
	rg := group(found.ID)
	if !ValidGroup(rg) || !ValidAddressName(found.Name) {
		return publicIP{}, fmt.Errorf("Azure named address %s in a way Argus cannot address", id)
	}
	return publicIP{Name: found.Name, Group: rg, Path: addressPath(subscription, rg, found.Name), IP: found.Properties.IPAddress, Tags: found.Tags,
		IPConfig: found.Properties.IPConfiguration.ID, Gateway: found.Properties.NATGateway.ID != ""}, nil
}

// nicPath is the path of the network interface arm names, rebuilt from its
// checked parts, when arm is an interface (or something under one) in this
// subscription.
func nicPath(subscription, arm string) (string, bool) {
	parts := strings.Split(strings.Trim(arm, "/"), "/")
	if len(parts) < 8 || !strings.EqualFold(parts[0], "subscriptions") || !strings.EqualFold(parts[1], subscription) ||
		!strings.EqualFold(parts[2], "resourceGroups") || !strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], "Microsoft.Network") || !strings.EqualFold(parts[6], "networkInterfaces") {
		return "", false
	}
	if !ValidGroup(parts[3]) || !reNICName.MatchString(parts[7]) {
		return "", false
	}
	return SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(parts[3]) + "/providers/Microsoft.Network/networkInterfaces/" + url.PathEscape(parts[7]), true
}

// nic is a network interface as Azure returned it, kept whole so that
// writing it back changes only what Argus changed.
type nic struct {
	body map[string]any
	etag string
	// configs are its IP configurations, the same maps body holds.
	configs []map[string]any
}

func (p *Provider) readNIC(ctx context.Context, t token, actor cloud.Actor, path string) (nic, error) {
	var n nic
	if err := p.get(ctx, t, actor, p.ep.arm+path+"?api-version="+networkAPI, &n.body); err != nil {
		return nic{}, err
	}
	n.etag, _ = n.body["etag"].(string)
	if n.etag == "" {
		return nic{}, errors.New("Azure gave the network interface no etag, so Argus cannot change it safely")
	}
	props, _ := n.body["properties"].(map[string]any)
	list, _ := props["ipConfigurations"].([]any)
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			n.configs = append(n.configs, m)
		}
	}
	if len(n.configs) == 0 {
		return nic{}, errors.New("the network interface has no IP configuration")
	}
	return n, nil
}

func configProps(c map[string]any) map[string]any {
	props, _ := c["properties"].(map[string]any)
	return props
}

// addressOn is the path of the public address an IP configuration has.
func addressOn(c map[string]any) string {
	ref, _ := configProps(c)["publicIPAddress"].(map[string]any)
	id, _ := ref["id"].(string)
	return id
}

// primaryConfig is the interface's primary IP configuration, or its only
// one.
func (n nic) primaryConfig() map[string]any {
	for _, c := range n.configs {
		if primary, _ := configProps(c)["primary"].(bool); primary {
			return c
		}
	}
	if len(n.configs) == 1 {
		return n.configs[0]
	}
	return nil
}

func (p *Provider) writeNIC(ctx context.Context, t token, actor cloud.Actor, path string, n nic) (string, error) {
	return p.sendIf(ctx, t, actor, http.MethodPut, p.ep.arm+path+"?api-version="+networkAPI, n.etag, n.body, nil)
}

// AllocateAddress makes a Standard static IPv4 address. ref is
// AddressRef(resource group, name).
func (p *Provider) AllocateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, ref string) (cloud.Result, error) {
	rg, name, ok := strings.Cut(ref, "/")
	if !ok || !ValidGroup(rg) || !ValidAddressName(name) {
		return cloud.Result{}, fmt.Errorf("%q does not name an address in a resource group", ref)
	}
	if place(region) != region || region == "" {
		return cloud.Result{}, fmt.Errorf("%q is not a region", region)
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	path := addressPath(a.AccountID, rg, name)
	u := p.ep.arm + path + "?api-version=" + networkAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return cloud.Result{}, err
	} else if there {
		return cloud.Result{}, fmt.Errorf("resource group %s already has a public address named %s", rg, name)
	}
	body := map[string]any{"location": region, "sku": map[string]string{"name": "Standard"},
		"properties": map[string]string{"publicIPAllocationMethod": "Static", "publicIPAddressVersion": "IPv4"}}
	var made raw
	req, err := p.send(ctx, t, actor, http.MethodPut, u, body, &made)
	if err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: strings.ToLower(made.Properties.ResourceGUID), RequestID: req, Detail: "Made " + name + " in " + rg}
	if !ValidGUID(res.ResourceID) {
		res.ResourceID = pathID(path)
	}
	if made.Properties.IPAddress != "" {
		res.Detail += ": " + made.Properties.IPAddress
	}
	return res, nil
}

// AssociateAddress puts an address on a VM's primary network interface.
func (p *Provider) AssociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address, instance string) (cloud.Result, error) {
	t, m, err := p.manage(ctx, a, actor, region, instance)
	if err != nil {
		return cloud.Result{}, err
	}
	ip, err := p.locateAddress(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	if ip.IPConfig != "" || ip.Gateway {
		return cloud.Result{}, fmt.Errorf("%s is attached to something; take it off first", ip.Name)
	}
	if m.NIC == "" {
		return cloud.Result{}, fmt.Errorf("Argus cannot tell which of %s's network interfaces is its primary one", m.Name)
	}
	path, ok := nicPath(a.AccountID, m.NIC)
	if !ok {
		return cloud.Result{}, fmt.Errorf("Azure named %s's network interface in a way Argus cannot address", m.Name)
	}
	n, err := p.readNIC(ctx, t, actor, path)
	if err != nil {
		return cloud.Result{}, err
	}
	c := n.primaryConfig()
	if c == nil {
		return cloud.Result{}, fmt.Errorf("Argus cannot tell which of %s's IP configurations is its primary one", m.Name)
	}
	if has := addressOn(c); has != "" {
		return cloud.Result{}, fmt.Errorf("%s already has the public address %s; take that one off first", m.Name, last(has))
	}
	configProps(c)["publicIPAddress"] = map[string]any{"id": ip.Path}
	req, err := p.writeNIC(ctx, t, actor, path, n)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: address, RequestID: req}, nil
}

// DisassociateAddress takes an address off the network interface that has
// it. One a load balancer or a gateway has is left to the portal.
func (p *Provider) DisassociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	ip, err := p.locateAddress(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	if ip.IPConfig == "" && !ip.Gateway {
		return cloud.Result{}, fmt.Errorf("%s is not attached to anything", ip.Name)
	}
	path, ok := nicPath(a.AccountID, ip.IPConfig)
	if !ok {
		return cloud.Result{}, fmt.Errorf("%w: %s is on a load balancer or a gateway, not a VM's network interface; take it off in the Azure portal", cloud.ErrUnsupported, ip.Name)
	}
	n, err := p.readNIC(ctx, t, actor, path)
	if err != nil {
		return cloud.Result{}, err
	}
	taken := false
	for _, c := range n.configs {
		if strings.EqualFold(addressOn(c), ip.Path) {
			delete(configProps(c), "publicIPAddress")
			taken = true
		}
	}
	if !taken {
		return cloud.Result{}, fmt.Errorf("%s is no longer on that network interface", ip.Name)
	}
	req, err := p.writeNIC(ctx, t, actor, path, n)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: address, RequestID: req}, nil
}

// ReleaseAddress deletes an address nothing uses.
func (p *Provider) ReleaseAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	ip, err := p.locateAddress(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	if ip.IPConfig != "" || ip.Gateway {
		return cloud.Result{}, fmt.Errorf("%s is attached; take it off first", ip.Name)
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.arm+ip.Path+"?api-version="+networkAPI, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: address, RequestID: req}, nil
}
