package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Managed disks and their snapshots. A disk is known to Argus by its
uniqueId, which Azure does not take in a path, so a change lists the
subscription's disks to find the one with that id, the same way a VM is
found, and builds the path from its resource group and name.

Attaching and detaching go through the VM's attachDetachDataDisks
operation and not through a rewrite of the VM's list of data disks: Azure
adds or removes the one disk on its side, so a disk someone else attaches
at the same moment is not dropped.

A new disk or snapshot is made with a PUT at a path holding its name, and
Azure takes a PUT at a name already in use as a change to what is there. So
Argus looks first and refuses a name in use: it never overwrites a disk or
a snapshot it did not make.

Nothing here encrypts with a key of the customer's: a disk is encrypted
with a key Azure manages, as the portal's default is.
*/

// DiskTypes are the disk types Argus makes, the cheaper SSD first. Ultra
// and Premium SSD v2 disks take a performance of their own and are made in
// the portal.
var DiskTypes = []string{"StandardSSD_LRS", "Premium_LRS", "Standard_LRS", "StandardSSD_ZRS", "Premium_ZRS"}

// The sizes a managed disk of DiskTypes has, in GiB.
const (
	MinDiskGB = 1
	MaxDiskGB = 32767
)

var reDiskName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,78}[A-Za-z0-9_])?$`)

// ValidDiskName reports whether n can name a disk or a snapshot: up to 80
// letters, digits, . _ and -, starting with a letter or digit and ending
// with a letter, digit or _.
func ValidDiskName(n string) bool { return reDiskName.MatchString(n) }

// ValidGroup reports whether g can be a resource group's name in a path.
func ValidGroup(g string) bool { return reGroupName.MatchString(g) && !strings.HasSuffix(g, ".") }

// ZoneNumber is the number Azure knows one of a region's zones by: eastus-2
// is zone 2 of eastus.
func ZoneNumber(region, zone string) (string, bool) {
	n, ok := strings.CutPrefix(zone, region+"-")
	if !ok || len(n) != 1 || n[0] < '1' || n[0] > '3' {
		return "", false
	}
	return n, true
}

// held is one disk or snapshot as a change needs it.
type held struct {
	Name     string
	Path     string // built by Argus from the checked group and name
	Group    string
	Location string
	Tags     map[string]string
	SizeGB   int
	OS       bool
	// VM is the path of the VM a disk is attached to, built by Argus, and
	// VMName its name; both empty for a disk nothing uses.
	VM, VMName string
}

// locateHeld finds the disk or snapshot (collection "disks" or
// "snapshots") with uniqueId id in the region the console named.
func (p *Provider) locateHeld(ctx context.Context, t token, actor cloud.Actor, subscription, region, collection, id string) (held, error) {
	what := strings.TrimSuffix(collection, "s")
	if !ValidGUID(id) {
		return held{}, fmt.Errorf("%q is not a %s id", id, what)
	}
	var found *raw
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Compute/"+collection+"?api-version="+diskAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for i := range items {
				if strings.EqualFold(items[i].Properties.UniqueID, id) {
					found = &items[i]
				}
			}
			return nil
		})
	if err != nil {
		return held{}, err
	}
	if found == nil {
		return held{}, fmt.Errorf("subscription %s has no %s with id %s", subscription, what, id)
	}
	if got := place(found.Location); got != region {
		return held{}, fmt.Errorf("%s is in %s, not %s", found.Name, got, region)
	}
	rg := group(found.ID)
	if !ValidGroup(rg) || !ValidDiskName(found.Name) {
		return held{}, fmt.Errorf("Azure named %s %s in a way Argus cannot address", what, id)
	}
	h := held{Name: found.Name, Group: rg, Location: region, Tags: found.Tags, SizeGB: found.Properties.DiskSizeGB,
		OS: found.Properties.OSType != "", Path: heldPath(subscription, rg, collection, found.Name)}
	if by := found.ManagedBy; by != "" {
		vg, vn := group(by), last(by)
		if !strings.Contains(strings.ToLower(by), "/providers/microsoft.compute/virtualmachines/") || !ValidGroup(vg) || !reVMName.MatchString(vn) {
			return held{}, fmt.Errorf("%s is in use by something Argus cannot address: %s", found.Name, by)
		}
		h.VMName = vn
		h.VM = SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(vg) + "/providers/Microsoft.Compute/virtualMachines/" + url.PathEscape(vn)
	}
	return h, nil
}

func heldPath(subscription, rg, collection, name string) string {
	return SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Compute/" + collection + "/" + url.PathEscape(name)
}

// exists reports whether Azure has something at path u: a 200 is yes, a
// 404 no, and anything else is Azure's own refusal.
func (p *Provider) exists(ctx context.Context, t token, actor cloud.Actor, u string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	resp, err := p.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if v != nil {
			_ = json.Unmarshal(b, v)
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, azureError(resp.StatusCode, b)
}

// pollEvery and readyBudget are how often and how long Argus looks for a
// new disk to be ready before attaching it.
var (
	pollEvery   = 2 * time.Second
	readyBudget = 90 * time.Second
)

// CreateVolume makes a managed disk in s.Group, empty or from a snapshot,
// and attaches it when asked to.
func (p *Provider) CreateVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.VolumeSpec) (cloud.Result, error) {
	switch {
	case !ValidDiskName(s.Name):
		return cloud.Result{}, fmt.Errorf("%q cannot name a disk", s.Name)
	case !ValidGroup(s.Group):
		return cloud.Result{}, fmt.Errorf("%q is not a resource group", s.Group)
	case !slices.Contains(DiskTypes, s.Type):
		return cloud.Result{}, fmt.Errorf("%q is not a disk type Argus makes", s.Type)
	case s.SizeGB < MinDiskGB || s.SizeGB > MaxDiskGB:
		return cloud.Result{}, fmt.Errorf("a disk is %d to %d GiB", MinDiskGB, MaxDiskGB)
	case s.IOPS != 0 || s.Throughput != 0:
		return cloud.Result{}, errors.New("Argus makes Azure disks with their type's own performance")
	}
	for k, v := range s.Tags {
		if why := TagProblem(k, v); why != "" {
			return cloud.Result{}, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	body := map[string]any{"location": region, "sku": map[string]string{"name": s.Type}}
	if s.Zone != "" {
		n, ok := ZoneNumber(region, s.Zone)
		if !ok {
			return cloud.Result{}, fmt.Errorf("%q is not one of %s's zones", s.Zone, region)
		}
		body["zones"] = []string{n}
	}
	if len(s.Tags) > 0 {
		body["tags"] = s.Tags
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	creation := map[string]string{"createOption": "Empty"}
	if s.Snapshot != "" {
		snap, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "snapshots", s.Snapshot)
		if err != nil {
			return cloud.Result{}, err
		}
		creation = map[string]string{"createOption": "Copy", "sourceResourceId": snap.Path}
	}
	body["properties"] = map[string]any{"diskSizeGB": s.SizeGB, "creationData": creation}
	var target vm
	if s.AttachTo != "" {
		if target, err = p.locateVM(ctx, t, actor, a.AccountID, region, s.AttachTo); err != nil {
			return cloud.Result{}, err
		}
	}
	path := heldPath(a.AccountID, s.Group, "disks", s.Name)
	u := p.ep.arm + path + "?api-version=" + diskAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return cloud.Result{}, err
	} else if there {
		return cloud.Result{}, fmt.Errorf("resource group %s already has a disk named %s", s.Group, s.Name)
	}
	var made raw
	req, err := p.send(ctx, t, actor, http.MethodPut, u, body, &made)
	if err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: strings.ToLower(made.Properties.UniqueID), RequestID: req, Detail: "Made " + s.Name + " in " + s.Group}
	if s.AttachTo == "" {
		if !ValidGUID(res.ResourceID) {
			res.ResourceID = pathID(path)
		}
		return res, nil
	}
	// Azure attaches only a disk it has finished making.
	deadline := time.Now().Add(readyBudget)
	for {
		var now raw
		there, err := p.exists(ctx, t, actor, u, &now)
		if err == nil && there && strings.EqualFold(now.Properties.ProvisioningState, "Succeeded") {
			if id := strings.ToLower(now.Properties.UniqueID); ValidGUID(id) {
				res.ResourceID = id
			}
			break
		}
		if err == nil && there && strings.EqualFold(now.Properties.ProvisioningState, "Failed") {
			return res, fmt.Errorf("Azure could not make %s; look at it in the portal", s.Name)
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("%s is made but was not ready to attach in time; attach it to %s once it is", s.Name, target.Name)
		}
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("%s is made but not attached: %w", s.Name, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
	if !ValidGUID(res.ResourceID) {
		res.ResourceID = pathID(path)
	}
	if _, err := p.attachDetach(ctx, t, actor, target.Path, path, true); err != nil {
		return res, fmt.Errorf("%s is made but not attached to %s: %w", s.Name, target.Name, err)
	}
	res.Detail += ", attached to " + target.Name
	return res, nil
}

// ModifyVolume grows a disk. Azure changes a disk's type only while
// nothing uses it, and Argus leaves that to the portal.
func (p *Provider) ModifyVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, c cloud.VolumeChange) (cloud.Result, error) {
	if c.Type != "" || c.IOPS != 0 || c.Throughput != 0 {
		return cloud.Result{}, errNotYet("change a disk's type or performance")
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "disks", id)
	if err != nil {
		return cloud.Result{}, err
	}
	if int(c.SizeGB) <= d.SizeGB {
		return cloud.Result{}, fmt.Errorf("Azure only grows a disk; %s is %d GiB", d.Name, d.SizeGB)
	}
	if c.SizeGB > MaxDiskGB {
		return cloud.Result{}, fmt.Errorf("a disk is at most %d GiB", MaxDiskGB)
	}
	req, err := p.send(ctx, t, actor, http.MethodPatch, p.ep.arm+d.Path+"?api-version="+diskAPI,
		map[string]any{"properties": map[string]any{"diskSizeGB": c.SizeGB}}, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

// attachDetach attaches the disk at path disk to the VM at path vmPath, or
// takes it off.
func (p *Provider) attachDetach(ctx context.Context, t token, actor cloud.Actor, vmPath, disk string, attach bool) (string, error) {
	key := "dataDisksToDetach"
	if attach {
		key = "dataDisksToAttach"
	}
	return p.send(ctx, t, actor, http.MethodPost, p.ep.arm+vmPath+"/attachDetachDataDisks?api-version="+computeAPI,
		map[string]any{key: []map[string]string{{"diskId": disk}}}, nil)
}

// AttachVolume attaches a disk to a VM as a data disk. Azure picks the
// LUN, the lowest the VM has free; device is not used.
func (p *Provider) AttachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, instance, _ string) (cloud.Result, error) {
	t, m, err := p.manage(ctx, a, actor, region, instance)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	if d.VM != "" {
		return cloud.Result{}, fmt.Errorf("%s is attached to %s; detach it first", d.Name, d.VMName)
	}
	req, err := p.attachDetach(ctx, t, actor, m.Path, d.Path, true)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: req}, nil
}

// DetachVolume takes a data disk off the VM that has it.
func (p *Provider) DetachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	if d.VM == "" {
		return cloud.Result{}, fmt.Errorf("%s is not attached", d.Name)
	}
	if d.OS {
		return cloud.Result{}, fmt.Errorf("%s is the disk %s starts from; Azure does not detach that one", d.Name, d.VMName)
	}
	req, err := p.attachDetach(ctx, t, actor, d.VM, d.Path, false)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: req}, nil
}

// DeleteVolume deletes a disk nothing uses.
func (p *Provider) DeleteVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string, finalSnapshot bool) (cloud.Result, error) {
	if finalSnapshot {
		return cloud.Result{}, errors.New("in Azure a final snapshot is its own change: take a snapshot first, then delete the disk")
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	if d.VM != "" {
		return cloud.Result{}, fmt.Errorf("%s is attached to %s; detach it first", d.Name, d.VMName)
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.arm+d.Path+"?api-version="+diskAPI, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: req}, nil
}

// CreateSnapshot takes an incremental snapshot of a disk, next to it in
// its resource group. Azure keeps an incremental snapshot as a full copy
// to restore from and bills only what changed since the one before.
func (p *Provider) CreateSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, name string) (cloud.Result, error) {
	if !ValidDiskName(name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a snapshot", name)
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	path := heldPath(a.AccountID, d.Group, "snapshots", name)
	u := p.ep.arm + path + "?api-version=" + diskAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return cloud.Result{}, err
	} else if there {
		return cloud.Result{}, fmt.Errorf("resource group %s already has a snapshot named %s", d.Group, name)
	}
	body := map[string]any{"location": region, "properties": map[string]any{"incremental": true,
		"creationData": map[string]string{"createOption": "Copy", "sourceResourceId": d.Path}}}
	req, err := p.send(ctx, t, actor, http.MethodPut, u, body, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: name, RequestID: req}, nil
}

// DeleteSnapshot deletes a snapshot.
func (p *Provider) DeleteSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, region, snapshot string) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	s, err := p.locateHeld(ctx, t, actor, a.AccountID, region, "snapshots", snapshot)
	if err != nil {
		return cloud.Result{}, err
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.arm+s.Path+"?api-version="+diskAPI, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: snapshot, RequestID: req}, nil
}

// Images come to Azure later; Supports says so before either of these is
// asked.

func (p *Provider) CreateImage(context.Context, cloud.Account, cloud.Actor, string, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("make an image")
}

func (p *Provider) DeleteImage(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("delete an image")
}

// needs are the manage role's actions each change takes, beyond reading
// the VM it is about.
var needs = map[cloud.Action][]string{
	cloud.ActionVolumeCreate:   {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/write", "Microsoft.Compute/snapshots/read", "Microsoft.Compute/snapshots/beginGetAccess/action", "Microsoft.Compute/virtualMachines/attachDetachDataDisks/action"},
	cloud.ActionVolumeGrow:     {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/write"},
	cloud.ActionVolumeAttach:   {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/write", "Microsoft.Compute/virtualMachines/attachDetachDataDisks/action"},
	cloud.ActionVolumeDetach:   {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/write", "Microsoft.Compute/virtualMachines/attachDetachDataDisks/action"},
	cloud.ActionVolumeDelete:   {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/delete"},
	cloud.ActionSnapshotCreate: {"Microsoft.Compute/disks/read", "Microsoft.Compute/disks/beginGetAccess/action", "Microsoft.Compute/snapshots/read", "Microsoft.Compute/snapshots/write"},
	cloud.ActionSnapshotDelete: {"Microsoft.Compute/snapshots/read", "Microsoft.Compute/snapshots/delete"},

	cloud.ActionAddressAllocate:     {"Microsoft.Network/publicIPAddresses/read", "Microsoft.Network/publicIPAddresses/write"},
	cloud.ActionAddressAssociate:    {"Microsoft.Network/publicIPAddresses/read", "Microsoft.Network/publicIPAddresses/join/action", "Microsoft.Network/networkInterfaces/read", "Microsoft.Network/networkInterfaces/write"},
	cloud.ActionAddressDisassociate: {"Microsoft.Network/publicIPAddresses/read", "Microsoft.Network/networkInterfaces/read", "Microsoft.Network/networkInterfaces/write"},
	cloud.ActionAddressRelease:      {"Microsoft.Network/publicIPAddresses/read", "Microsoft.Network/publicIPAddresses/delete"},

	cloud.ActionFirewallCreate:    {"Microsoft.Network/networkSecurityGroups/read", "Microsoft.Network/networkSecurityGroups/write"},
	cloud.ActionFirewallRules:     {"Microsoft.Network/networkSecurityGroups/read", "Microsoft.Network/networkSecurityGroups/write"},
	cloud.ActionFirewallDelete:    {"Microsoft.Network/networkSecurityGroups/read", "Microsoft.Network/networkSecurityGroups/delete"},
	cloud.ActionInstanceFirewalls: {"Microsoft.Network/networkSecurityGroups/read", "Microsoft.Network/networkSecurityGroups/join/action", "Microsoft.Network/networkInterfaces/read", "Microsoft.Network/networkInterfaces/write"},

	cloud.ActionProtect:   {"Microsoft.Authorization/locks/write"},
	cloud.ActionUnprotect: {"Microsoft.Authorization/locks/delete"},

	cloud.ActionLaunch: {"Microsoft.Compute/virtualMachines/write", "Microsoft.Compute/disks/write", "Microsoft.Compute/images/read",
		"Microsoft.Network/networkInterfaces/write", "Microsoft.Network/networkInterfaces/join/action",
		"Microsoft.Network/publicIPAddresses/write", "Microsoft.Network/publicIPAddresses/join/action",
		"Microsoft.Network/virtualNetworks/subnets/join/action", "Microsoft.Network/networkSecurityGroups/join/action"},
	cloud.ActionRemove: {"Microsoft.Compute/virtualMachines/delete", "Microsoft.Network/networkInterfaces/delete", "Microsoft.Compute/disks/delete",
		"Microsoft.Network/publicIPAddresses/delete", "Microsoft.Compute/disks/beginGetAccess/action", "Microsoft.Compute/snapshots/write"},
}

// Needs is the manage role's actions a change takes.
func (p *Provider) Needs(a cloud.Action) []string { return needs[a] }

// roleAPI is the Microsoft.Authorization version role definitions are
// read with.
const roleAPI = "2022-04-01"

// ManageMissing is which of ManageActions a subscription's manage role
// does not allow: a role made before Argus could do something lacks that
// thing's actions until the setup command is run again. The role is read
// with the Reader assignment.
func (p *Provider) ManageMissing(ctx context.Context, read cloud.Account, manageRole string) ([]string, error) {
	if manageRole != RoleID(read.AccountID, ManageRoleID(read.AccountID)) {
		return nil, fmt.Errorf("%s is not this subscription's Argus manage role", manageRole)
	}
	t, _, err := p.session(ctx, read, "argus")
	if err != nil {
		return nil, err
	}
	var def struct {
		Properties struct {
			Permissions []struct {
				Actions []string `json:"actions"`
			} `json:"permissions"`
		} `json:"properties"`
	}
	if err := p.get(ctx, t, "argus", p.ep.arm+manageRole+"?api-version="+roleAPI, &def); err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, perm := range def.Properties.Permissions {
		for _, a := range perm.Actions {
			has[strings.ToLower(a)] = true
		}
	}
	missing := []string{}
	for _, a := range ManageActions {
		if !has[strings.ToLower(a)] {
			missing = append(missing, a)
		}
	}
	return missing, nil
}

// SnapshotName is a snapshot's name when none is given: the disk's name
// and the time.
func SnapshotName(disk string, at time.Time) string {
	stamp := at.Format("20060102-1504")
	if len(disk) > 80-len(stamp)-1 {
		disk = strings.TrimRight(disk[:80-len(stamp)-1], "._-")
	}
	return disk + "-" + stamp
}

var (
	_ cloud.Storage       = (*Provider)(nil)
	_ cloud.PolicyChecker = (*Provider)(nil)
)
