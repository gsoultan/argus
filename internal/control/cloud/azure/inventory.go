package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Azure lists a kind across a whole subscription in one call, not per region.
The sync asks one region at a time, so one read of the subscription is kept
for a minute and every region is cut from it: a sync of sixty regions is
nine list calls, not hundreds.

Ids. Azure names a resource by a path (subscription, resource group, name),
and a name can be used again once the resource is gone. Where Azure also
gives a resource an id of its own that is never used twice (a VM's vmId, a
disk's uniqueId, a network's resourceGuid) that is the id here, so a change
waiting for a decision can never land on a different VM that took the old
one's name. An image and a subnet have none: theirs is the path after the
subscription, in lower case. The path itself is kept in the details as
resourceId, with the resource group, for whatever acts on the resource.
*/

// inventoryTTL is how long one read of a subscription serves its regions.
const inventoryTTL = time.Minute

// Kinds is what Argus reads in a subscription. Every one is in a region.
var Kinds = []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindFirewall, cloud.KindNetwork, cloud.KindSubnet,
	cloud.KindAddress, cloud.KindSnapshot, cloud.KindImage, cloud.KindVPN}

type subscriptionInventory struct {
	mu   sync.Mutex
	at   time.Time
	all  []cloud.Resource
	err  error
	read bool
}

// Inventory reads one region of a subscription.
func (p *Provider) Inventory(ctx context.Context, a cloud.Account, region string, kinds []cloud.Kind) (cloud.Snapshot, error) {
	t, _, err := p.session(ctx, a, cloud.SyncActor)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	key := a.AccountID + "|" + a.RoleARN + "|" + t.oid
	p.mu.Lock()
	si, ok := p.inv[key]
	if !ok {
		if len(p.inv) >= maxSessions {
			for k, v := range p.inv {
				if time.Since(v.at) > inventoryTTL || len(p.inv) >= maxSessions {
					delete(p.inv, k)
				}
			}
		}
		si = &subscriptionInventory{}
		p.inv[key] = si
	}
	p.mu.Unlock()

	si.mu.Lock()
	if !si.read || time.Since(si.at) > inventoryTTL {
		si.all, si.err = p.readSubscription(ctx, t, a.AccountID)
		si.at, si.read = time.Now(), true
	}
	all, rerr, at := si.all, si.err, si.at
	si.mu.Unlock()
	if rerr != nil {
		return cloud.Snapshot{}, rerr
	}

	var read []cloud.Kind
	for _, k := range kinds {
		if slices.Contains(Kinds, k) {
			read = append(read, k)
		}
	}
	out := cloud.Snapshot{Region: region, Kinds: read, ReadAt: at.UTC()}
	for _, r := range all {
		if r.Region == region && slices.Contains(read, r.Kind) {
			out.Resources = append(out.Resources, r)
		}
	}
	return out, nil
}

type idRef struct {
	ID string `json:"id"`
}

// raw is the parts of Azure's resources Argus maps. One struct for every
// kind: each fills the fields it has.
type raw struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Location  string            `json:"location"`
	Zones     []string          `json:"zones"`
	Tags      map[string]string `json:"tags"`
	ManagedBy string            `json:"managedBy"`
	SKU       struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		ProvisioningState string `json:"provisioningState"`
		TimeCreated       string `json:"timeCreated"`
		ResourceGUID      string `json:"resourceGuid"`
		UniqueID          string `json:"uniqueId"`

		// Virtual machines.
		VMID            string `json:"vmId"`
		Priority        string `json:"priority"`
		HardwareProfile struct {
			VMSize string `json:"vmSize"`
		} `json:"hardwareProfile"`
		OSProfile struct {
			ComputerName string `json:"computerName"`
		} `json:"osProfile"`
		StorageProfile struct {
			ImageReference struct {
				Publisher string `json:"publisher"`
				Offer     string `json:"offer"`
				SKU       string `json:"sku"`
			} `json:"imageReference"`
			OSDisk struct {
				Name        string `json:"name"`
				OSType      string `json:"osType"`
				DiskSizeGB  int    `json:"diskSizeGB"`
				ManagedDisk idRef  `json:"managedDisk"`
			} `json:"osDisk"`
			DataDisks []struct {
				Name        string `json:"name"`
				Lun         int    `json:"lun"`
				ManagedDisk idRef  `json:"managedDisk"`
			} `json:"dataDisks"`
		} `json:"storageProfile"`
		NetworkProfile struct {
			NetworkInterfaces []struct {
				ID         string `json:"id"`
				Properties struct {
					Primary *bool `json:"primary"`
				} `json:"properties"`
			} `json:"networkInterfaces"`
		} `json:"networkProfile"`
		VMScaleSet   idRef `json:"virtualMachineScaleSet"`
		InstanceView struct {
			Statuses []struct {
				Code string `json:"code"`
			} `json:"statuses"`
		} `json:"instanceView"`

		// Disks and snapshots.
		DiskSizeGB   int    `json:"diskSizeGB"`
		DiskState    string `json:"diskState"`
		OSType       string `json:"osType"`
		Incremental  bool   `json:"incremental"`
		CreationData struct {
			CreateOption     string `json:"createOption"`
			SourceResourceID string `json:"sourceResourceId"`
		} `json:"creationData"`

		// Images.
		SourceVM         idRef  `json:"sourceVirtualMachine"`
		HyperVGeneration string `json:"hyperVGeneration"`

		// Network interfaces.
		VirtualMachine   idRef `json:"virtualMachine"`
		NSG              idRef `json:"networkSecurityGroup"`
		IPConfigurations []struct {
			ID         string `json:"id"`
			Properties struct {
				PrivateIPAddress string `json:"privateIPAddress"`
				Primary          bool   `json:"primary"`
				Subnet           idRef  `json:"subnet"`
				PublicIPAddress  idRef  `json:"publicIPAddress"`
			} `json:"properties"`
		} `json:"ipConfigurations"`

		// Public IP addresses.
		IPAddress        string `json:"ipAddress"`
		AllocationMethod string `json:"publicIPAllocationMethod"`
		AddressVersion   string `json:"publicIPAddressVersion"`
		IPConfiguration  idRef  `json:"ipConfiguration"`
		NATGateway       idRef  `json:"natGateway"`

		// Virtual networks.
		AddressSpace struct {
			AddressPrefixes []string `json:"addressPrefixes"`
		} `json:"addressSpace"`
		Subnets []rawSubnet `json:"subnets"`

		// Network security groups.
		SecurityRules []rawRule `json:"securityRules"`

		// Virtual network gateway connections (sharedKey is never read).
		ConnectionStatus       string `json:"connectionStatus"`
		ConnectionType         string `json:"connectionType"`
		EnableBGP              bool   `json:"enableBgp"`
		VirtualNetworkGateway1 idRef  `json:"virtualNetworkGateway1"`
		VirtualNetworkGateway2 idRef  `json:"virtualNetworkGateway2"`
		LocalNetworkGateway2   idRef  `json:"localNetworkGateway2"`
		IngressBytes           int64  `json:"ingressBytesTransferred"`
		EgressBytes            int64  `json:"egressBytesTransferred"`
		TunnelConnectionStatus []struct {
			Tunnel           string `json:"tunnel"`
			ConnectionStatus string `json:"connectionStatus"`
		} `json:"tunnelConnectionStatus"`

		// Local network gateways: the far end of a site-to-site connection.
		GatewayIPAddress         string `json:"gatewayIpAddress"`
		LocalNetworkAddressSpace struct {
			AddressPrefixes []string `json:"addressPrefixes"`
		} `json:"localNetworkAddressSpace"`
	} `json:"properties"`
}

type rawSubnet struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		AddressPrefix   string   `json:"addressPrefix"`
		AddressPrefixes []string `json:"addressPrefixes"`
		NSG             idRef    `json:"networkSecurityGroup"`
	} `json:"properties"`
}

type rawRule struct {
	Name       string `json:"name"`
	Properties struct {
		Description                string   `json:"description"`
		Protocol                   string   `json:"protocol"`
		Access                     string   `json:"access"`
		Direction                  string   `json:"direction"`
		Priority                   int      `json:"priority"`
		SourceAddressPrefix        string   `json:"sourceAddressPrefix"`
		SourceAddressPrefixes      []string `json:"sourceAddressPrefixes"`
		SourceASGs                 []idRef  `json:"sourceApplicationSecurityGroups"`
		DestinationAddressPrefix   string   `json:"destinationAddressPrefix"`
		DestinationAddressPrefixes []string `json:"destinationAddressPrefixes"`
		DestinationASGs            []idRef  `json:"destinationApplicationSecurityGroups"`
		DestinationPortRange       string   `json:"destinationPortRange"`
		DestinationPortRanges      []string `json:"destinationPortRanges"`
	} `json:"properties"`
}

// API versions, each one generally available for its resource provider.
const (
	computeAPI = "2024-07-01"
	diskAPI    = "2024-03-02"
	networkAPI = "2024-05-01"
)

// readSubscription reads every kind in a subscription.
func (p *Provider) readSubscription(ctx context.Context, t token, subscription string) ([]cloud.Resource, error) {
	base := p.ep.arm + SubscriptionScope(subscription) + "/providers/"
	const (
		lVMs = iota
		lStatus
		lDisks
		lSnapshots
		lImages
		lNICs
		lAddresses
		lNetworks
		lNSGs
		lLocks
		lConnections
		lLocalGateways
	)
	lists := []string{
		lVMs:           "Microsoft.Compute/virtualMachines?api-version=" + computeAPI,
		lStatus:        "Microsoft.Compute/virtualMachines?api-version=" + computeAPI + "&statusOnly=true",
		lDisks:         "Microsoft.Compute/disks?api-version=" + diskAPI,
		lSnapshots:     "Microsoft.Compute/snapshots?api-version=" + diskAPI,
		lImages:        "Microsoft.Compute/images?api-version=" + computeAPI,
		lNICs:          "Microsoft.Network/networkInterfaces?api-version=" + networkAPI,
		lAddresses:     "Microsoft.Network/publicIPAddresses?api-version=" + networkAPI,
		lNetworks:      "Microsoft.Network/virtualNetworks?api-version=" + networkAPI,
		lNSGs:          "Microsoft.Network/networkSecurityGroups?api-version=" + networkAPI,
		lLocks:         "Microsoft.Authorization/locks?api-version=" + lockAPI,
		lConnections:   "Microsoft.Network/connections?api-version=" + networkAPI,
		lLocalGateways: "Microsoft.Network/localNetworkGateways?api-version=" + networkAPI,
	}
	got := make([][]raw, len(lists))
	errs := make([]error, len(lists))
	var wg sync.WaitGroup
	for n, l := range lists {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[n], errs[n] = p.list(ctx, t, base+l)
		}()
	}
	wg.Wait()
	// The locks only say which VMs are protected. An inventory without
	// them is still an inventory: no VM then says whether it is locked.
	locks, locksRead := lockIndex(got[lLocks]), errs[lLocks] == nil
	errs[lLocks] = nil
	// VPN connections only add to the inventory: a role made before they
	// were read, or a subscription without the network provider, still
	// has the rest.
	vpnsRead := errs[lConnections] == nil
	errs[lConnections], errs[lLocalGateways] = nil, nil
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// Everything refers to everything else by path; Argus refers by id.
	ids := map[string]cloud.Ref{}
	note := func(kind cloud.Kind, r raw) {
		ids[strings.ToLower(r.ID)] = cloud.Ref{Kind: kind, ID: resourceID(kind, r), Name: r.Name}
	}
	for _, r := range got[lVMs] {
		note(cloud.KindInstance, r)
	}
	for _, r := range got[lDisks] {
		note(cloud.KindVolume, r)
	}
	for _, r := range got[lSnapshots] {
		note(cloud.KindSnapshot, r)
	}
	for _, r := range got[lAddresses] {
		note(cloud.KindAddress, r)
	}
	for _, r := range got[lNSGs] {
		note(cloud.KindFirewall, r)
	}
	subnets := map[string]rawSubnet{}
	for _, r := range got[lNetworks] {
		note(cloud.KindNetwork, r)
		for _, s := range r.Properties.Subnets {
			ids[strings.ToLower(s.ID)] = cloud.Ref{Kind: cloud.KindSubnet, ID: pathID(s.ID), Name: s.Name}
			subnets[strings.ToLower(s.ID)] = s
		}
	}
	// A VM's power state comes from the second read; one the read did not
	// name has no state said for it, rather than a guess.
	power := map[string]string{}
	for _, r := range got[lStatus] {
		for _, s := range r.Properties.InstanceView.Statuses {
			if st, ok := strings.CutPrefix(s.Code, "PowerState/"); ok {
				power[strings.ToLower(r.ID)] = st
			}
		}
	}
	nics := map[string]raw{}
	byIPConfig := map[string]raw{} // the interface an IP configuration is on
	for _, r := range got[lNICs] {
		nics[strings.ToLower(r.ID)] = r
		for _, c := range r.Properties.IPConfigurations {
			byIPConfig[strings.ToLower(c.ID)] = r
		}
	}
	addresses := map[string]raw{}
	for _, r := range got[lAddresses] {
		addresses[strings.ToLower(r.ID)] = r
	}

	var out []cloud.Resource
	for _, r := range got[lVMs] {
		vm := instance(r, ids, power, nics, addresses, subnets)
		if locksRead {
			locks.mark(vm.Details, r.ID)
		}
		out = append(out, vm)
	}
	for _, r := range got[lDisks] {
		out = append(out, disk(r, ids))
	}
	for _, r := range got[lSnapshots] {
		out = append(out, snapshot(r, ids))
	}
	for _, r := range got[lImages] {
		out = append(out, image(r, ids))
	}
	for _, r := range got[lAddresses] {
		out = append(out, address(r, ids, byIPConfig))
	}
	for _, r := range got[lNetworks] {
		out = append(out, network(r))
		for _, s := range r.Properties.Subnets {
			out = append(out, subnet(r, s, ids))
		}
	}
	// What each security group is on: interfaces and subnets.
	use := map[string]groupUse{}
	for _, r := range got[lNICs] {
		if g := strings.ToLower(r.Properties.NSG.ID); g != "" {
			u := use[g]
			u.interfaces++
			use[g] = u
		}
	}
	for _, s := range subnets {
		if g := strings.ToLower(s.Properties.NSG.ID); g != "" {
			u := use[g]
			u.subnets = append(u.subnets, s.ID)
			use[g] = u
		}
	}
	for _, r := range got[lNSGs] {
		u := use[strings.ToLower(r.ID)]
		sort.Strings(u.subnets)
		out = append(out, securityGroup(r, ids, u))
	}
	if vpnsRead {
		lngs := map[string]raw{}
		for _, r := range got[lLocalGateways] {
			lngs[strings.ToLower(r.ID)] = r
		}
		for _, r := range got[lConnections] {
			out = append(out, vpnConnection(r, lngs))
		}
	}
	return out, nil
}

// vpnConnection maps a virtual network gateway connection. An active-active
// gateway reports each tunnel; otherwise the connection is its one tunnel.
func vpnConnection(r raw, lngs map[string]raw) cloud.Resource {
	p := r.Properties
	res := cloud.Resource{Kind: cloud.KindVPN, ID: pathID(r.ID), Name: r.Name, Region: place(r.Location),
		State: strings.ToLower(p.ProvisioningState), Tags: r.Tags, Details: map[string]string{}}
	set(res.Details, "resourceId", r.ID)
	set(res.Details, cloud.DetailGateway, last(p.VirtualNetworkGateway1.ID))
	set(res.Details, cloud.DetailVPNType, p.ConnectionType)
	if p.EnableBGP {
		set(res.Details, cloud.DetailRouting, "bgp")
	} else {
		set(res.Details, cloud.DetailRouting, "static")
	}
	if l, ok := lngs[strings.ToLower(p.LocalNetworkGateway2.ID)]; ok {
		set(res.Details, cloud.DetailRemoteAddress, l.Properties.GatewayIPAddress)
		set(res.Details, cloud.DetailRemoteNetworks, strings.Join(l.Properties.LocalNetworkAddressSpace.AddressPrefixes, ","))
	} else if p.VirtualNetworkGateway2.ID != "" {
		set(res.Details, cloud.DetailRemoteAddress, last(p.VirtualNetworkGateway2.ID))
	}
	set(res.Details, "ingressBytes", strconv.FormatInt(p.IngressBytes, 10))
	set(res.Details, "egressBytes", strconv.FormatInt(p.EgressBytes, 10))
	if !strings.EqualFold(p.ProvisioningState, "Succeeded") {
		return res
	}
	var ts []cloud.Tunnel
	for _, t := range p.TunnelConnectionStatus {
		ts = append(ts, cloud.Tunnel{Address: t.Tunnel, Up: t.ConnectionStatus == "Connected", Why: t.ConnectionStatus})
	}
	if len(ts) == 0 && p.ConnectionStatus != "" {
		ts = append(ts, cloud.Tunnel{Address: res.Details[cloud.DetailRemoteAddress], Up: p.ConnectionStatus == "Connected", Why: p.ConnectionStatus})
	}
	if len(ts) == 0 {
		// The list does not always carry a status; without one Argus
		// says nothing rather than "down".
		res.State = "unknown"
		return res
	}
	cloud.SetTunnels(&res, ts)
	return res
}

// list reads every page of one list.
func (p *Provider) list(ctx context.Context, t token, u string) ([]raw, error) {
	var out []raw
	err := p.pages(ctx, t, cloud.SyncActor, u, func(v json.RawMessage) error {
		var items []raw
		if err := json.Unmarshal(v, &items); err != nil {
			return err
		}
		out = append(out, items...)
		return nil
	})
	if err != nil {
		what := u[strings.LastIndex(u, "/")+1:]
		what, _, _ = strings.Cut(what, "?")
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// pathID is a resource's path after its subscription, in lower case and
// without the words Azure puts between the names:
// /subscriptions/s/resourceGroups/Web/providers/Microsoft.Compute/images/Base
// is web/base, and a subnet is group/network/subnet.
func pathID(arm string) string {
	parts := strings.Split(strings.Trim(strings.ToLower(arm), "/"), "/")
	var out []string
	for i := 0; i+1 < len(parts); {
		switch parts[i] {
		case "subscriptions":
			i += 2
		case "providers":
			// providers/<namespace>, then <type>/<name> pairs.
			i += 2
		default:
			out = append(out, parts[i+1])
			i += 2
		}
	}
	return strings.Join(out, "/")
}

// group is the resource group in a resource's path.
func group(arm string) string {
	parts := strings.Split(strings.Trim(arm, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

// last is a path's last segment: the name it ends in.
func last(u string) string { return u[strings.LastIndex(u, "/")+1:] }

// resourceID is the id Argus keeps for a resource: Azure's own
// never-reused id where it has one, else its path.
func resourceID(kind cloud.Kind, r raw) string {
	id := ""
	switch kind {
	case cloud.KindInstance:
		id = r.Properties.VMID
	case cloud.KindVolume, cloud.KindSnapshot:
		id = r.Properties.UniqueID
	case cloud.KindFirewall, cloud.KindNetwork, cloud.KindAddress:
		id = r.Properties.ResourceGUID
	}
	if id = strings.ToLower(id); ValidGUID(id) {
		return id
	}
	return pathID(r.ID)
}

func ref(ids map[string]cloud.Ref, arm string, kind cloud.Kind) cloud.Ref {
	if r, ok := ids[strings.ToLower(arm)]; ok {
		return r
	}
	return cloud.Ref{Kind: kind, ID: pathID(arm), Name: last(arm)}
}

func created(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func set(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func gib(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n) + " GiB"
}

// zone is an availability zone as Argus keeps one: Azure numbers them
// inside a region, so eastus's zone 1 is eastus-1.
func zone(region string, zones []string) string {
	if len(zones) == 0 || zones[0] == "" {
		return ""
	}
	return region + "-" + zones[0]
}

// common fills what every kind has.
func common(kind cloud.Kind, r raw) cloud.Resource {
	region := place(r.Location)
	res := cloud.Resource{Kind: kind, ID: resourceID(kind, r), Name: r.Name, Region: region, Zone: zone(region, r.Zones),
		CreatedAt: created(r.Properties.TimeCreated), Tags: r.Tags, Details: map[string]string{}}
	set(res.Details, "resourceGroup", group(r.ID))
	set(res.Details, "resourceId", r.ID)
	return res
}

// instanceState is Azure's power state in the words the console already
// uses for instances. A VM that is stopped from inside is still allocated
// and still billed; one that is deallocated is not. Both are "stopped"
// here, and the details say which.
func instanceState(power string) string {
	switch power {
	case "running":
		return "running"
	case "starting":
		return "pending"
	case "stopping", "deallocating":
		return "stopping"
	case "stopped", "deallocated":
		return "stopped"
	}
	return power
}

func instance(r raw, ids map[string]cloud.Ref, power map[string]string, nics, addresses map[string]raw, subnets map[string]rawSubnet) cloud.Resource {
	res := common(cloud.KindInstance, r)
	pr := r.Properties
	res.Size = pr.HardwareProfile.VMSize
	if st, ok := power[strings.ToLower(r.ID)]; ok {
		res.State = instanceState(st)
		res.Details["powerState"] = st
		if st == "stopped" {
			res.Details["allocated"] = "yes"
		}
	}
	link := func(l cloud.Ref) {
		if !slices.ContainsFunc(res.Links, func(x cloud.Ref) bool { return x.Kind == l.Kind && x.ID == l.ID }) {
			res.Links = append(res.Links, l)
		}
	}
	// The primary interface first: its address is the VM's own.
	list := slices.Clone(pr.NetworkProfile.NetworkInterfaces)
	sort.SliceStable(list, func(i, j int) bool {
		pi, pj := list[i].Properties.Primary, list[j].Properties.Primary
		return pi != nil && *pi && !(pj != nil && *pj)
	})
	for _, n := range list {
		nic, ok := nics[strings.ToLower(n.ID)]
		if !ok {
			continue
		}
		for _, c := range nic.Properties.IPConfigurations {
			if c.Properties.PrivateIPAddress != "" {
				res.Addresses = append(res.Addresses, c.Properties.PrivateIPAddress)
			}
			if a, ok := addresses[strings.ToLower(c.Properties.PublicIPAddress.ID)]; ok && a.Properties.IPAddress != "" && res.PublicIP == "" {
				res.PublicIP = a.Properties.IPAddress
			}
			if sid := c.Properties.Subnet.ID; sid != "" {
				link(ref(ids, sid, cloud.KindSubnet))
				if res.Network == "" {
					res.Network = ref(ids, networkOfSubnet(sid), cloud.KindNetwork).ID
				}
				// A subnet's security group applies to everything in it.
				if s, ok := subnets[strings.ToLower(sid)]; ok && s.Properties.NSG.ID != "" {
					link(ref(ids, s.Properties.NSG.ID, cloud.KindFirewall))
				}
			}
		}
		if nic.Properties.NSG.ID != "" {
			link(ref(ids, nic.Properties.NSG.ID, cloud.KindFirewall))
		}
	}
	if d := pr.StorageProfile.OSDisk; d.ManagedDisk.ID != "" {
		l := ref(ids, d.ManagedDisk.ID, cloud.KindVolume)
		l.Name = d.Name
		link(l)
		// The name the console already reads for the disk that boots it.
		res.Details["rootDeviceName"] = d.Name
	}
	for _, d := range pr.StorageProfile.DataDisks {
		if d.ManagedDisk.ID != "" {
			l := ref(ids, d.ManagedDisk.ID, cloud.KindVolume)
			l.Name = d.Name
			link(l)
		}
	}
	set(res.Details, "platform", strings.ToLower(pr.StorageProfile.OSDisk.OSType))
	set(res.Details, "computerName", pr.OSProfile.ComputerName)
	if im := pr.StorageProfile.ImageReference; im.Offer != "" {
		res.Details["image"] = strings.Trim(im.Publisher+" "+im.Offer+" "+im.SKU, " ")
	}
	if strings.EqualFold(pr.Priority, "Spot") {
		res.Details["lifecycle"] = "spot"
	}
	if pr.VMScaleSet.ID != "" {
		res.Details["scaleSet"] = last(pr.VMScaleSet.ID)
	}
	set(res.Details, "status", pr.ProvisioningState)
	return res
}

// networkOfSubnet is the virtual network a subnet's path is under.
func networkOfSubnet(subnet string) string {
	if i := strings.LastIndex(strings.ToLower(subnet), "/subnets/"); i > 0 {
		return subnet[:i]
	}
	return subnet
}

func disk(r raw, ids map[string]cloud.Ref) cloud.Resource {
	res := common(cloud.KindVolume, r)
	res.Size = gib(r.Properties.DiskSizeGB)
	res.State = "available"
	if r.ManagedBy != "" {
		res.State = "in-use"
		res.Links = append(res.Links, ref(ids, r.ManagedBy, cloud.KindInstance))
	}
	set(res.Details, "type", r.SKU.Name)
	set(res.Details, "diskState", r.Properties.DiskState)
	set(res.Details, "platform", strings.ToLower(r.Properties.OSType))
	set(res.Details, "status", r.Properties.ProvisioningState)
	return res
}

// settled is Azure's provisioning state as a resource's state: done is the
// word the console already has for that kind, anything else is Azure's own
// in lower case, which reads as on its way.
func settled(provisioning, done string) string {
	if provisioning == "" || strings.EqualFold(provisioning, "Succeeded") {
		return done
	}
	return strings.ToLower(provisioning)
}

func snapshot(r raw, ids map[string]cloud.Ref) cloud.Resource {
	res := common(cloud.KindSnapshot, r)
	res.Size = gib(r.Properties.DiskSizeGB)
	res.State = settled(r.Properties.ProvisioningState, "completed")
	if src := r.Properties.CreationData.SourceResourceID; strings.Contains(strings.ToLower(src), "/disks/") {
		res.Links = append(res.Links, ref(ids, src, cloud.KindVolume))
	}
	set(res.Details, "type", r.SKU.Name)
	if r.Properties.Incremental {
		res.Details["incremental"] = "yes"
	}
	return res
}

func image(r raw, ids map[string]cloud.Ref) cloud.Resource {
	res := common(cloud.KindImage, r)
	res.Size = gib(r.Properties.StorageProfile.OSDisk.DiskSizeGB)
	res.State = settled(r.Properties.ProvisioningState, "available")
	if src := r.Properties.SourceVM.ID; src != "" {
		res.Links = append(res.Links, ref(ids, src, cloud.KindInstance))
	}
	set(res.Details, "platform", strings.ToLower(r.Properties.StorageProfile.OSDisk.OSType))
	set(res.Details, "generation", r.Properties.HyperVGeneration)
	return res
}

func address(r raw, ids map[string]cloud.Ref, byIPConfig map[string]raw) cloud.Resource {
	res := common(cloud.KindAddress, r)
	pr := r.Properties
	if pr.IPAddress != "" {
		res.Addresses = []string{pr.IPAddress}
		res.PublicIP = pr.IPAddress
	}
	switch {
	case pr.IPConfiguration.ID != "" || pr.NATGateway.ID != "":
		res.State = "attached"
		// On a VM's interface; a load balancer's or a gateway's is attached
		// too, to something Argus does not list here.
		if nic, ok := byIPConfig[strings.ToLower(pr.IPConfiguration.ID)]; ok && nic.Properties.VirtualMachine.ID != "" {
			res.Links = append(res.Links, ref(ids, nic.Properties.VirtualMachine.ID, cloud.KindInstance))
		}
	default:
		// Azure charges for a standard address nothing uses.
		res.State = "unattached"
	}
	set(res.Details, "type", r.SKU.Name)
	set(res.Details, "allocation", strings.ToLower(pr.AllocationMethod))
	set(res.Details, "version", pr.AddressVersion)
	return res
}

func network(r raw) cloud.Resource {
	res := common(cloud.KindNetwork, r)
	res.State = "available"
	res.Size = strings.Join(r.Properties.AddressSpace.AddressPrefixes, ", ")
	return res
}

func subnet(nw raw, s rawSubnet, ids map[string]cloud.Ref) cloud.Resource {
	region := place(nw.Location)
	cidr := s.Properties.AddressPrefix
	if cidr == "" {
		cidr = strings.Join(s.Properties.AddressPrefixes, ", ")
	}
	res := cloud.Resource{Kind: cloud.KindSubnet, ID: pathID(s.ID), Name: s.Name, Region: region, Size: cidr,
		Network: resourceID(cloud.KindNetwork, nw), State: "available", Details: map[string]string{}}
	set(res.Details, "resourceGroup", group(s.ID))
	set(res.Details, "resourceId", s.ID)
	res.Details["network"] = nw.Name
	if s.Properties.NSG.ID != "" {
		res.Links = append(res.Links, ref(ids, s.Properties.NSG.ID, cloud.KindFirewall))
	}
	return res
}

// groupUse is what a network security group is on.
type groupUse struct {
	interfaces int
	subnets    []string
}

// securityGroup is a network security group as a firewall.
//
// Azure's rules are ordered by priority and can deny, which the neutral
// rule list cannot say. So the list holds what the group lets through: the
// allow rules, less any a deny rule ahead of it shuts completely. A deny
// that shuts only part of an allow leaves the allow listed whole, which
// errs towards showing a port as open rather than hiding one. Azure's own
// default rules (inside the virtual network and from its load balancer in,
// everything out, the rest denied) are in every group and are not listed.
func securityGroup(r raw, ids map[string]cloud.Ref, users groupUse) cloud.Resource {
	res := common(cloud.KindFirewall, r)
	res.State = "enabled"
	denies := 0
	for _, rule := range r.Properties.SecurityRules {
		if !strings.EqualFold(rule.Properties.Access, "Allow") {
			denies++
		}
	}
	for _, l := range listed(r.Properties.SecurityRules) {
		res.Rules = append(res.Rules, l.rules...)
	}
	if denies > 0 {
		res.Details["denyRules"] = strconv.Itoa(denies)
	}
	// What uses the group, counted from the interfaces and subnets read in
	// this same pass, so the number is known and not Azure's back-reference.
	for _, s := range users.subnets {
		res.Links = append(res.Links, ref(ids, s, cloud.KindSubnet))
	}
	res.Details["interfaces"] = strconv.Itoa(users.interfaces + len(users.subnets))
	return res
}

// listedRule is one of a group's allow rules with the neutral rules it
// comes to.
type listedRule struct {
	name  string
	rules []cloud.Rule
}

// listed is the allow rules of a group that are not shut by a deny-all
// ahead of them, in priority order, each with what it lets through.
func listed(all []rawRule) []listedRule {
	rules := slices.Clone(all)
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Properties.Priority < rules[j].Properties.Priority })
	var out []listedRule
	for n, rule := range rules {
		if !strings.EqualFold(rule.Properties.Access, "Allow") {
			continue
		}
		inbound := !strings.EqualFold(rule.Properties.Direction, "Outbound")
		shut := false
		for _, d := range rules[:n] {
			shut = shut || denyAll(d, inbound)
		}
		if !shut {
			out = append(out, listedRule{name: rule.Name, rules: allowed(rule)})
		}
	}
	return out
}

// allowed is what one allow rule lets through: a neutral rule for each of
// its sources and each of its spans of ports.
func allowed(rule rawRule) []cloud.Rule {
	pr := rule.Properties
	inbound := !strings.EqualFold(pr.Direction, "Outbound")
	// The side a rule limits: where traffic comes from on the way in,
	// where it goes on the way out.
	prefixes, asgs := append(slices.Clone(pr.SourceAddressPrefixes), pr.SourceAddressPrefix), pr.SourceASGs
	if !inbound {
		prefixes, asgs = append(slices.Clone(pr.DestinationAddressPrefixes), pr.DestinationAddressPrefix), pr.DestinationASGs
	}
	var sources []string
	for _, s := range prefixes {
		if s = strings.TrimSpace(s); s != "" {
			sources = append(sources, source(s))
		}
	}
	for _, g := range asgs {
		sources = append(sources, "group:"+last(g.ID))
	}
	proto := protocol(pr.Protocol)
	spans := [][2]int32{{0, 65535}}
	if proto == "icmp" {
		spans = [][2]int32{{-1, -1}}
	} else {
		var named [][2]int32
		for _, ps := range append(slices.Clone(pr.DestinationPortRanges), pr.DestinationPortRange) {
			if from, to, ok := portSpan(strings.TrimSpace(ps)); ok {
				named = append(named, [2]int32{from, to})
			}
		}
		if len(named) > 0 {
			spans = named
		}
	}
	desc := rule.Name
	if pr.Description != "" {
		desc += ": " + pr.Description
	}
	var out []cloud.Rule
	for _, src := range sources {
		for _, sp := range spans {
			out = append(out, cloud.Rule{Inbound: inbound, Protocol: proto, FromPort: sp[0], ToPort: sp[1],
				Source: src, Description: desc})
		}
	}
	return out
}

// denyAll reports whether a rule denies everything in one direction.
func denyAll(r rawRule, inbound bool) bool {
	pr := r.Properties
	if !strings.EqualFold(pr.Access, "Deny") || inbound == strings.EqualFold(pr.Direction, "Outbound") {
		return false
	}
	return pr.Protocol == "*" && pr.SourceAddressPrefix == "*" && pr.DestinationAddressPrefix == "*" &&
		pr.DestinationPortRange == "*" && len(pr.SourceASGs) == 0 && len(pr.DestinationASGs) == 0
}

// source is an Azure address prefix in the neutral rule's terms: anywhere
// is 0.0.0.0/0, so the checks that look for the whole internet see it; a
// service tag (VirtualNetwork, AzureLoadBalancer, Storage.WestEurope) is
// marked as one; a range or an address is itself.
func source(s string) string {
	switch strings.ToLower(s) {
	case "*", "internet", "any", "0.0.0.0/0":
		return "0.0.0.0/0"
	case "::/0":
		return "::/0"
	}
	if c := s[0]; (c >= '0' && c <= '9') || strings.Contains(s, ":") {
		return s
	}
	return "tag:" + s
}

func protocol(p string) string {
	switch strings.ToLower(p) {
	case "*":
		return "all"
	}
	return strings.ToLower(p)
}

func portSpan(s string) (int32, int32, bool) {
	if s == "" {
		return 0, 0, false
	}
	if s == "*" {
		return 0, 65535, true
	}
	a, b, found := strings.Cut(s, "-")
	from, err := strconv.ParseInt(a, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	to := from
	if found {
		if to, err = strconv.ParseInt(b, 10, 32); err != nil {
			return 0, 0, false
		}
	}
	return int32(from), int32(to), true
}
