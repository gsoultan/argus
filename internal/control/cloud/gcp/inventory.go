package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Google lists most of Compute Engine project-wide (aggregatedList), not per
region. The sync asks one region at a time, so one read of the whole
project is kept for a minute and every region is cut from it: a sync of
forty regions is six list calls, not hundreds.
*/

// inventoryTTL is how long one read of a project serves its regions.
const inventoryTTL = time.Minute

// maxPages bounds one list, at 500 a page.
const maxPages = 40

// RegionalKinds and GlobalKinds are what a region, and "global", hold.
var (
	RegionalKinds = []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindSubnet, cloud.KindAddress}
	GlobalKinds   = []cloud.Kind{cloud.KindFirewall, cloud.KindNetwork, cloud.KindSnapshot, cloud.KindImage, cloud.KindAddress}
)

type projectInventory struct {
	mu   sync.Mutex
	at   time.Time
	all  []cloud.Resource
	err  error
	read bool
}

// Inventory reads one region of a project, or "global".
func (p *Provider) Inventory(ctx context.Context, a cloud.Account, region string, kinds []cloud.Kind) (cloud.Snapshot, error) {
	t, err := p.session(ctx, a, cloud.SyncActor)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	key := a.AccountID + "|" + a.RoleARN
	p.mu.Lock()
	pi, ok := p.inv[key]
	if !ok {
		if len(p.inv) >= maxSessions {
			for k, v := range p.inv {
				if time.Since(v.at) > inventoryTTL {
					delete(p.inv, k)
				}
			}
		}
		pi = &projectInventory{}
		p.inv[key] = pi
	}
	p.mu.Unlock()

	pi.mu.Lock()
	if !pi.read || time.Since(pi.at) > inventoryTTL {
		pi.all, pi.err = p.readProject(ctx, t, a.AccountID)
		pi.at, pi.read = time.Now(), true
	}
	all, rerr, at := pi.all, pi.err, pi.at
	pi.mu.Unlock()
	if rerr != nil {
		return cloud.Snapshot{}, rerr
	}

	hold := RegionalKinds
	if region == GlobalRegion {
		hold = GlobalKinds
	}
	var read []cloud.Kind
	for _, k := range kinds {
		if slices.Contains(hold, k) {
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

// raw is the parts of Compute Engine's resources Argus maps. One struct for
// every kind: each fills the fields it has.
type raw struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	SelfLink          string            `json:"selfLink"`
	Zone              string            `json:"zone"`
	Region            string            `json:"region"`
	Status            string            `json:"status"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Description       string            `json:"description"`
	// Instances.
	MachineType       string `json:"machineType"`
	NetworkInterfaces []struct {
		NetworkIP     string `json:"networkIP"`
		Network       string `json:"network"`
		Subnetwork    string `json:"subnetwork"`
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
	Disks []struct {
		Source     string `json:"source"`
		DeviceName string `json:"deviceName"`
		Boot       bool   `json:"boot"`
	} `json:"disks"`
	Tags struct {
		Items []string `json:"items"`
	} `json:"tags"`
	ServiceAccounts []struct {
		Email string `json:"email"`
	} `json:"serviceAccounts"`
	CPUPlatform string `json:"cpuPlatform"`
	Scheduling  struct {
		Preemptible       bool   `json:"preemptible"`
		ProvisioningModel string `json:"provisioningModel"`
	} `json:"scheduling"`
	// Disks, snapshots and images.
	SizeGb     string   `json:"sizeGb"`
	DiskSizeGb string   `json:"diskSizeGb"`
	Type       string   `json:"type"`
	Users      []string `json:"users"`
	SourceDisk string   `json:"sourceDisk"`
	// Addresses.
	Address     string `json:"address"`
	AddressType string `json:"addressType"`
	// Subnetworks and firewalls.
	Network        string `json:"network"`
	IPCidrRange    string `json:"ipCidrRange"`
	GatewayAddress string `json:"gatewayAddress"`
	// Networks.
	AutoCreateSubnetworks bool `json:"autoCreateSubnetworks"`
	// Firewalls.
	Direction         string   `json:"direction"`
	Priority          int      `json:"priority"`
	Disabled          bool     `json:"disabled"`
	SourceRanges      []string `json:"sourceRanges"`
	DestinationRanges []string `json:"destinationRanges"`
	SourceTags        []string `json:"sourceTags"`
	TargetTags        []string `json:"targetTags"`
	TargetSAs         []string `json:"targetServiceAccounts"`
	Allowed           []struct {
		Protocol string   `json:"IPProtocol"`
		Ports    []string `json:"ports"`
	} `json:"allowed"`
	Denied []struct {
		Protocol string `json:"IPProtocol"`
	} `json:"denied"`
}

// readProject reads every kind in a project.
func (p *Provider) readProject(ctx context.Context, t token, project string) ([]cloud.Resource, error) {
	base := p.ep.compute + "/compute/v1/projects/" + project
	type list struct {
		kind cloud.Kind
		path string
		// field is the aggregated list's key per scope; "" is a plain list.
		field string
	}
	lists := []list{
		{cloud.KindInstance, "/aggregated/instances", "instances"},
		{cloud.KindVolume, "/aggregated/disks", "disks"},
		{cloud.KindSubnet, "/aggregated/subnetworks", "subnetworks"},
		{cloud.KindAddress, "/aggregated/addresses", "addresses"},
		{cloud.KindAddress, "/global/addresses", ""},
		{cloud.KindNetwork, "/global/networks", ""},
		{cloud.KindFirewall, "/global/firewalls", ""},
		{cloud.KindSnapshot, "/global/snapshots", ""},
		{cloud.KindImage, "/global/images", ""},
	}
	got := make([][]raw, len(lists))
	errs := make([]error, len(lists))
	var wg sync.WaitGroup
	for n, l := range lists {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[n], errs[n] = p.list(ctx, t, base+l.path, l.field)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// Everything refers to everything else by URL; Argus refers by id.
	ids := map[string]cloud.Ref{}
	for n, l := range lists {
		for _, r := range got[n] {
			ids[trimLink(r.SelfLink)] = cloud.Ref{Kind: l.kind, ID: r.ID, Name: r.Name}
		}
	}
	var firewalls []raw
	for n, l := range lists {
		if l.kind == cloud.KindFirewall {
			firewalls = got[n]
		}
	}
	var out []cloud.Resource
	for n, l := range lists {
		for _, r := range got[n] {
			var res cloud.Resource
			switch l.kind {
			case cloud.KindInstance:
				res = instance(r, ids, firewalls)
			case cloud.KindVolume:
				res = disk(r, ids)
			case cloud.KindSubnet:
				res = subnet(r, ids)
			case cloud.KindAddress:
				res = address(r, ids)
			case cloud.KindNetwork:
				res = network(r)
			case cloud.KindFirewall:
				res = firewall(r, ids)
			case cloud.KindSnapshot:
				res = snapshot(r, ids)
			case cloud.KindImage:
				res = image(r)
			}
			out = append(out, res)
		}
	}
	return out, nil
}

// list reads every page of one list. An aggregated list has its items per
// zone or region under field.
func (p *Provider) list(ctx context.Context, t token, u, field string) ([]raw, error) {
	var out []raw
	page := ""
	for range maxPages {
		q := url.Values{"maxResults": {"500"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var resp struct {
			Items         json.RawMessage `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := p.get(ctx, t, cloud.SyncActor, u+"?"+q.Encode(), &resp); err != nil {
			return nil, fmt.Errorf("%s: %w", u[strings.LastIndex(u, "/")+1:], err)
		}
		if len(resp.Items) > 0 {
			if field == "" {
				var items []raw
				if err := json.Unmarshal(resp.Items, &items); err != nil {
					return nil, err
				}
				out = append(out, items...)
			} else {
				var scoped map[string]map[string]json.RawMessage
				if err := json.Unmarshal(resp.Items, &scoped); err != nil {
					return nil, err
				}
				for _, s := range scoped {
					var items []raw
					if b, ok := s[field]; ok {
						if err := json.Unmarshal(b, &items); err != nil {
							return nil, err
						}
					}
					out = append(out, items...)
				}
			}
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
	return out, nil
}

// trimLink is a resource URL from "projects/" on, so links written with
// and without the API's version match.
func trimLink(u string) string {
	if i := strings.Index(u, "/projects/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// last is a URL's last segment: the name it ends in.
func last(u string) string { return u[strings.LastIndex(u, "/")+1:] }

// regionOfZone is us-central1 for us-central1-a.
func regionOfZone(z string) string {
	if i := strings.LastIndex(z, "-"); i > 0 {
		return z[:i]
	}
	return z
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

func gib(s string) string {
	if s == "" {
		return ""
	}
	return s + " GiB"
}

// instanceState is Compute Engine's status in the words the console
// already uses for instances.
func instanceState(s string) string {
	switch s {
	case "RUNNING":
		return "running"
	case "PROVISIONING", "STAGING", "REPAIRING":
		return "pending"
	case "STOPPING", "SUSPENDING":
		return "stopping"
	case "TERMINATED", "STOPPED", "SUSPENDED":
		return "stopped"
	}
	return strings.ToLower(s)
}

func ref(ids map[string]cloud.Ref, link string, kind cloud.Kind) cloud.Ref {
	if r, ok := ids[trimLink(link)]; ok {
		return r
	}
	return cloud.Ref{Kind: kind, ID: last(link), Name: last(link)}
}

func instance(r raw, ids map[string]cloud.Ref, firewalls []raw) cloud.Resource {
	zone := last(r.Zone)
	res := cloud.Resource{Kind: cloud.KindInstance, ID: r.ID, Name: r.Name, Region: regionOfZone(zone), Zone: zone,
		State: instanceState(r.Status), Size: last(r.MachineType), CreatedAt: created(r.CreationTimestamp),
		Tags: r.Labels, Details: map[string]string{}}
	var sas []string
	for _, s := range r.ServiceAccounts {
		sas = append(sas, s.Email)
	}
	for n, nic := range r.NetworkInterfaces {
		if nic.NetworkIP != "" {
			res.Addresses = append(res.Addresses, nic.NetworkIP)
		}
		for _, ac := range nic.AccessConfigs {
			if ac.NatIP != "" && res.PublicIP == "" {
				res.PublicIP = ac.NatIP
			}
		}
		if n == 0 && nic.Network != "" {
			res.Network = ref(ids, nic.Network, cloud.KindNetwork).ID
		}
		if nic.Subnetwork != "" {
			res.Links = append(res.Links, ref(ids, nic.Subnetwork, cloud.KindSubnet))
		}
		// A firewall applies to an instance by network, then by its
		// targets: none is every instance in the network.
		for _, f := range firewalls {
			if trimLink(f.Network) != trimLink(nic.Network) {
				continue
			}
			applies := len(f.TargetTags) == 0 && len(f.TargetSAs) == 0
			for _, tg := range f.TargetTags {
				applies = applies || slices.Contains(r.Tags.Items, tg)
			}
			for _, sa := range f.TargetSAs {
				applies = applies || slices.Contains(sas, sa)
			}
			if applies && !slices.ContainsFunc(res.Links, func(l cloud.Ref) bool { return l.Kind == cloud.KindFirewall && l.ID == f.ID }) {
				res.Links = append(res.Links, cloud.Ref{Kind: cloud.KindFirewall, ID: f.ID, Name: f.Name})
			}
		}
	}
	for _, d := range r.Disks {
		if d.Source != "" {
			l := ref(ids, d.Source, cloud.KindVolume)
			l.Name = d.DeviceName
			res.Links = append(res.Links, l)
		}
	}
	set(res.Details, "networkTags", strings.Join(r.Tags.Items, ", "))
	set(res.Details, "serviceAccount", strings.Join(sas, ", "))
	set(res.Details, "cpuPlatform", r.CPUPlatform)
	if r.Scheduling.Preemptible || r.Scheduling.ProvisioningModel == "SPOT" {
		res.Details["lifecycle"] = "spot"
	}
	set(res.Details, "status", r.Status)
	return res
}

func disk(r raw, ids map[string]cloud.Ref) cloud.Resource {
	zone := last(r.Zone)
	region := regionOfZone(zone)
	if zone == "" {
		region = last(r.Region) // a regional disk
	}
	res := cloud.Resource{Kind: cloud.KindVolume, ID: r.ID, Name: r.Name, Region: region, Zone: zone,
		Size: gib(r.SizeGb), CreatedAt: created(r.CreationTimestamp), Tags: r.Labels, State: "available", Details: map[string]string{}}
	if len(r.Users) > 0 {
		res.State = "in-use"
	}
	for _, u := range r.Users {
		res.Links = append(res.Links, ref(ids, u, cloud.KindInstance))
	}
	set(res.Details, "type", last(r.Type))
	set(res.Details, "status", r.Status)
	return res
}

func subnet(r raw, ids map[string]cloud.Ref) cloud.Resource {
	res := cloud.Resource{Kind: cloud.KindSubnet, ID: r.ID, Name: r.Name, Region: last(r.Region), Size: r.IPCidrRange,
		Network: ref(ids, r.Network, cloud.KindNetwork).ID, CreatedAt: created(r.CreationTimestamp), State: "available",
		Details: map[string]string{}}
	set(res.Details, "gateway", r.GatewayAddress)
	return res
}

func address(r raw, ids map[string]cloud.Ref) cloud.Resource {
	region := last(r.Region)
	if region == "" {
		region = GlobalRegion
	}
	res := cloud.Resource{Kind: cloud.KindAddress, ID: r.ID, Name: r.Name, Region: region, Addresses: []string{r.Address},
		CreatedAt: created(r.CreationTimestamp), Tags: r.Labels, Details: map[string]string{}}
	external := r.AddressType == "" || r.AddressType == "EXTERNAL"
	if external {
		res.PublicIP = r.Address
	}
	switch {
	case r.Status == "IN_USE":
		res.State = "attached"
	case external:
		// Google charges for an external address nothing uses.
		res.State = "unattached"
	default:
		res.State = "reserved"
	}
	for _, u := range r.Users {
		if strings.Contains(u, "/instances/") {
			res.Links = append(res.Links, ref(ids, u, cloud.KindInstance))
		}
	}
	set(res.Details, "type", strings.ToLower(r.AddressType))
	return res
}

func network(r raw) cloud.Resource {
	res := cloud.Resource{Kind: cloud.KindNetwork, ID: r.ID, Name: r.Name, Region: GlobalRegion, State: "available",
		CreatedAt: created(r.CreationTimestamp), Details: map[string]string{}}
	if r.AutoCreateSubnetworks {
		res.Details["subnetMode"] = "auto"
	} else {
		res.Details["subnetMode"] = "custom"
	}
	if r.Name == "default" {
		res.Details["default"] = "yes"
	}
	return res
}

func firewall(r raw, ids map[string]cloud.Ref) cloud.Resource {
	nw := ref(ids, r.Network, cloud.KindNetwork)
	res := cloud.Resource{Kind: cloud.KindFirewall, ID: r.ID, Name: r.Name, Region: GlobalRegion, Network: nw.ID,
		CreatedAt: created(r.CreationTimestamp), State: "enabled", Links: []cloud.Ref{nw}, Details: map[string]string{}}
	set(res.Details, "description", r.Description)
	set(res.Details, "direction", strings.ToLower(r.Direction))
	set(res.Details, "priority", strconv.Itoa(r.Priority))
	set(res.Details, "targetTags", strings.Join(r.TargetTags, ", "))
	set(res.Details, "targetServiceAccounts", strings.Join(r.TargetSAs, ", "))
	if len(r.Denied) > 0 {
		res.Details["action"] = "deny"
	}
	if r.Disabled {
		// A disabled rule lets nothing in, so it lists no rules to flag.
		res.State = "disabled"
		return res
	}
	if len(r.Denied) > 0 {
		return res
	}
	inbound := r.Direction != "EGRESS"
	sources := r.SourceRanges
	if !inbound {
		sources = r.DestinationRanges
	}
	for _, t := range r.SourceTags {
		sources = append(sources, "tag:"+t)
	}
	if len(sources) == 0 {
		// Google's own default for a rule that names no source.
		sources = []string{"0.0.0.0/0"}
	}
	for _, a := range r.Allowed {
		proto := protocol(a.Protocol)
		spans := [][2]int32{{0, 65535}}
		if proto == "icmp" {
			spans = [][2]int32{{-1, -1}}
		} else if len(a.Ports) > 0 {
			spans = nil
			for _, ps := range a.Ports {
				from, to, ok := portSpan(ps)
				if ok {
					spans = append(spans, [2]int32{from, to})
				}
			}
		}
		for _, src := range sources {
			for _, sp := range spans {
				res.Rules = append(res.Rules, cloud.Rule{Inbound: inbound, Protocol: proto, FromPort: sp[0], ToPort: sp[1],
					Source: src})
			}
		}
	}
	return res
}

func protocol(p string) string {
	switch strings.ToLower(p) {
	case "tcp", "6":
		return "tcp"
	case "udp", "17":
		return "udp"
	case "icmp", "1":
		return "icmp"
	case "all":
		return "all"
	}
	return strings.ToLower(p)
}

func portSpan(s string) (int32, int32, bool) {
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

func snapshot(r raw, ids map[string]cloud.Ref) cloud.Resource {
	res := cloud.Resource{Kind: cloud.KindSnapshot, ID: r.ID, Name: r.Name, Region: GlobalRegion,
		State: strings.ToLower(r.Status), Size: gib(r.DiskSizeGb), CreatedAt: created(r.CreationTimestamp), Tags: r.Labels,
		Details: map[string]string{}}
	if r.SourceDisk != "" {
		res.Links = append(res.Links, ref(ids, r.SourceDisk, cloud.KindVolume))
	}
	set(res.Details, "description", r.Description)
	return res
}

func image(r raw) cloud.Resource {
	res := cloud.Resource{Kind: cloud.KindImage, ID: r.ID, Name: r.Name, Region: GlobalRegion,
		State: strings.ToLower(r.Status), Size: gib(r.DiskSizeGb), CreatedAt: created(r.CreationTimestamp), Tags: r.Labels,
		Details: map[string]string{}}
	set(res.Details, "description", r.Description)
	return res
}
