package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Removing a VM. Azure deletes a VM by itself and leaves its disks, network
interfaces and public addresses behind, unless each was marked to be
deleted with it (its deleteOption), as the portal offers when a VM is
made. Argus does not change those marks: the plan reads them and says
what goes and what stays, so nobody finds a disk or an address still
billed after the VM is gone, or loses one they meant to keep.

What stands in for termination protection is a resource lock. Argus reads
the locks on the VM and refuses to remove one that has any; it holds no
right to take a lock off. A lock put on the resource group or the
subscription is not in that list, and Azure refuses the delete itself.
*/

// armPath is the path of the resource arm names, rebuilt from its checked
// parts, when arm is a resource of that provider and type in this
// subscription.
func armPath(subscription, arm, provider, kind string, name *regexp.Regexp) (string, bool) {
	parts := strings.Split(strings.Trim(arm, "/"), "/")
	if len(parts) != 8 || !strings.EqualFold(parts[0], "subscriptions") || !strings.EqualFold(parts[1], subscription) ||
		!strings.EqualFold(parts[2], "resourceGroups") || !strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], provider) || !strings.EqualFold(parts[6], kind) {
		return "", false
	}
	if !ValidGroup(parts[3]) || !name.MatchString(parts[7]) {
		return "", false
	}
	return SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(parts[3]) + "/providers/" + provider + "/" + kind + "/" + url.PathEscape(parts[7]), true
}

// ErrLocked is a VM with a resource lock on it.
var ErrLocked = errors.New("it has a lock in Azure; take the lock off in the Azure portal first, which Argus cannot do")

// lockAPI is the Microsoft.Authorization version locks are read with.
const lockAPI = "2016-09-01"

type vmDisk struct {
	Name         string `json:"name"`
	Lun          int    `json:"lun"`
	DiskSizeGB   int    `json:"diskSizeGB"`
	DeleteOption string `json:"deleteOption"`
	ManagedDisk  idRef  `json:"managedDisk"`
}

// vmWhole is a VM as a remove needs it.
type vmWhole struct {
	ManagedBy  string `json:"managedBy"`
	Properties struct {
		HardwareProfile struct {
			VMSize string `json:"vmSize"`
		} `json:"hardwareProfile"`
		StorageProfile struct {
			OSDisk    vmDisk   `json:"osDisk"`
			DataDisks []vmDisk `json:"dataDisks"`
		} `json:"storageProfile"`
		NetworkProfile struct {
			NetworkInterfaces []struct {
				ID         string `json:"id"`
				Properties struct {
					DeleteOption string `json:"deleteOption"`
				} `json:"properties"`
			} `json:"networkInterfaces"`
		} `json:"networkProfile"`
		VMScaleSet   idRef `json:"virtualMachineScaleSet"`
		InstanceView struct {
			Statuses []struct {
				Code string `json:"code"`
			} `json:"statuses"`
		} `json:"instanceView"`
	} `json:"properties"`
}

// goes is a disk Azure deletes with the VM: where it is, for a final
// snapshot.
type goes struct {
	id, name, group, path string
}

// removal is what a remove acts on, beside the plan people see.
type removal struct {
	vm vm
	// disks are the managed disks deleted with the VM.
	disks []goes
}

func deleted(option string) bool { return strings.EqualFold(option, "Delete") }

// planRemove reads a VM and everything a delete takes with it.
func (p *Provider) planRemove(ctx context.Context, t token, actor cloud.Actor, subscription, region, id string) (cloud.RemovePlan, removal, error) {
	m, err := p.locateVM(ctx, t, actor, subscription, region, id)
	if err != nil {
		return cloud.RemovePlan{}, removal{}, err
	}
	var whole vmWhole
	if err := p.get(ctx, t, actor, p.ep.arm+m.Path+"?api-version="+computeAPI+"&$expand=instanceView", &whole); err != nil {
		return cloud.RemovePlan{}, removal{}, err
	}
	pr := whole.Properties
	plan := cloud.RemovePlan{ID: id, Name: m.Name, Size: pr.HardwareProfile.VMSize, Volumes: []cloud.Volume{}}
	for _, s := range pr.InstanceView.Statuses {
		if st, ok := strings.CutPrefix(s.Code, "PowerState/"); ok {
			plan.State = instanceState(st)
		}
	}
	switch {
	case pr.VMScaleSet.ID != "":
		plan.ManagedBy = "scale set " + last(pr.VMScaleSet.ID)
	case whole.ManagedBy != "":
		plan.ManagedBy = last(whole.ManagedBy)
	}
	var locks struct {
		Value []struct {
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := p.get(ctx, t, actor, p.ep.arm+m.Path+"/providers/Microsoft.Authorization/locks?api-version="+lockAPI, &locks); err != nil {
		return cloud.RemovePlan{}, removal{}, fmt.Errorf("reading the locks on %s: %w", m.Name, err)
	}
	plan.Protected = len(locks.Value) > 0

	// The disks, as the console knows them: by uniqueId.
	type known struct {
		id string
		gb int
	}
	disks := map[string]known{}
	err = p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Compute/disks?api-version="+diskAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, d := range items {
				disks[strings.ToLower(d.ID)] = known{strings.ToLower(d.Properties.UniqueID), d.Properties.DiskSizeGB}
			}
			return nil
		})
	if err != nil {
		return cloud.RemovePlan{}, removal{}, err
	}
	rm := removal{vm: m}
	add := func(d vmDisk, device string, boot bool) error {
		v := cloud.Volume{ID: d.Name, Device: device, SizeGB: int32(d.DiskSizeGB), Deleted: deleted(d.DeleteOption), Description: d.Name}
		if boot {
			v.Description += " (boot)"
		}
		if d.ManagedDisk.ID == "" {
			// A disk kept as a blob in a storage account stays there.
			v.Deleted = false
			v.Description += ", not a managed disk"
			plan.Volumes = append(plan.Volumes, v)
			return nil
		}
		path, ok := armPath(subscription, d.ManagedDisk.ID, "Microsoft.Compute", "disks", reDiskName)
		k, found := disks[strings.ToLower(d.ManagedDisk.ID)]
		if !ok || !found || !ValidGUID(k.id) {
			return fmt.Errorf("Argus cannot tell which disk %s is, so it cannot say what removing %s takes", d.Name, m.Name)
		}
		v.ID = k.id
		if k.gb > 0 {
			v.SizeGB = int32(k.gb)
		}
		if v.Deleted {
			rm.disks = append(rm.disks, goes{id: k.id, name: last(path), group: group(path), path: path})
		}
		plan.Volumes = append(plan.Volumes, v)
		return nil
	}
	if pr.StorageProfile.OSDisk.Name != "" {
		if err := add(pr.StorageProfile.OSDisk, "os", true); err != nil {
			return cloud.RemovePlan{}, removal{}, err
		}
	}
	data := slices.Clone(pr.StorageProfile.DataDisks)
	slices.SortFunc(data, func(x, y vmDisk) int { return x.Lun - y.Lun })
	for _, d := range data {
		if err := add(d, fmt.Sprintf("lun %d", d.Lun), false); err != nil {
			return cloud.RemovePlan{}, removal{}, err
		}
	}

	// The public address: Azure deletes it with the VM only when both it
	// and the interface it is on are marked so.
	var addresses map[string]string
	for _, n := range pr.NetworkProfile.NetworkInterfaces {
		path, ok := nicPath(subscription, n.ID)
		if !ok {
			return cloud.RemovePlan{}, removal{}, fmt.Errorf("Azure named a network interface of %s in a way Argus cannot address", m.Name)
		}
		nicGoes := deleted(n.Properties.DeleteOption)
		if !nicGoes {
			plan.Stays = append(plan.Stays, "network interface "+last(path))
		}
		var got struct {
			Properties struct {
				IPConfigurations []struct {
					Properties struct {
						PublicIPAddress struct {
							ID         string `json:"id"`
							Properties struct {
								DeleteOption string `json:"deleteOption"`
							} `json:"properties"`
						} `json:"publicIPAddress"`
					} `json:"properties"`
				} `json:"ipConfigurations"`
			} `json:"properties"`
		}
		if err := p.get(ctx, t, actor, p.ep.arm+path+"?api-version="+networkAPI, &got); err != nil {
			return cloud.RemovePlan{}, removal{}, err
		}
		for _, c := range got.Properties.IPConfigurations {
			ref := c.Properties.PublicIPAddress
			if ref.ID == "" || plan.PublicIP != "" || plan.ElasticIP != "" {
				continue
			}
			apath, ok := armPath(subscription, ref.ID, "Microsoft.Network", "publicIPAddresses", reDiskName)
			if !ok {
				return cloud.RemovePlan{}, removal{}, fmt.Errorf("Azure named a public address of %s in a way Argus cannot address", m.Name)
			}
			if addresses == nil {
				addresses = map[string]string{}
				err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/publicIPAddresses?api-version="+networkAPI,
					func(v json.RawMessage) error {
						var items []raw
						if err := json.Unmarshal(v, &items); err != nil {
							return err
						}
						for _, x := range items {
							addresses[strings.ToLower(x.ID)] = x.Properties.IPAddress
						}
						return nil
					})
				if err != nil {
					return cloud.RemovePlan{}, removal{}, err
				}
			}
			// An address Azure has not given a number yet goes by its name.
			ip := addresses[strings.ToLower(ref.ID)]
			if ip == "" {
				ip = last(apath)
			}
			if nicGoes && deleted(ref.Properties.DeleteOption) {
				plan.PublicIP = ip
			} else {
				plan.ElasticIP = ip
			}
		}
	}
	return plan, rm, nil
}

// PlanRemove reads what deleting a VM takes with it: the disks and the
// public address marked to be deleted with it, and what stays behind.
func (p *Provider) PlanRemove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) (cloud.RemovePlan, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.RemovePlan{}, err
	}
	plan, _, err := p.planRemove(ctx, t, actor, a.AccountID, region, id)
	return plan, err
}

// snapshotBudget is how long Argus waits for one final snapshot.
var snapshotBudget = 60 * time.Second

// Remove deletes a VM, and Azure deletes with it what is marked so. A
// final snapshot of each disk that goes is taken first, when asked, and
// Argus waits for each to be there before anything is deleted.
func (p *Provider) Remove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, finalSnapshots bool) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	plan, rm, err := p.planRemove(ctx, t, actor, a.AccountID, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	if plan.Protected {
		return cloud.Result{}, ErrLocked
	}
	out := cloud.Result{ResourceID: id}
	if finalSnapshots {
		out.Snapshots = map[string]string{}
		for _, d := range rm.disks {
			name := SnapshotName(d.name+"-final", time.Now().UTC())
			if err := p.finalSnapshot(ctx, t, actor, a.AccountID, region, d, name, plan.Name); err != nil {
				return out, fmt.Errorf("nothing was removed: the final snapshot of %s failed: %w", d.name, err)
			}
			out.Snapshots[d.id] = name
		}
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.arm+rm.vm.Path+"?api-version="+computeAPI, nil, nil)
	if err != nil {
		if len(out.Snapshots) > 0 {
			names := make([]string, 0, len(out.Snapshots))
			for _, n := range out.Snapshots {
				names = append(names, n)
			}
			slices.Sort(names)
			return out, fmt.Errorf("the final snapshots were taken (%s), but the VM was not removed: %w", strings.Join(names, ", "), err)
		}
		return out, err
	}
	out.RequestID = req
	if len(plan.Stays) > 0 {
		out.Detail = "Left in Azure, at no charge: " + strings.Join(plan.Stays, ", ")
	}
	return out, nil
}

// finalSnapshot takes an incremental snapshot of a disk under name, next
// to it, and waits until Azure has it.
func (p *Provider) finalSnapshot(ctx context.Context, t token, actor cloud.Actor, subscription, region string, d goes, name, vmName string) error {
	u := p.ep.arm + heldPath(subscription, d.group, "snapshots", name) + "?api-version=" + diskAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return err
	} else if there {
		return fmt.Errorf("resource group %s already has a snapshot named %s", d.group, name)
	}
	body := map[string]any{"location": region, "tags": map[string]string{"argus-final-snapshot-of": vmName},
		"properties": map[string]any{"incremental": true, "creationData": map[string]string{"createOption": "Copy", "sourceResourceId": d.path}}}
	if _, err := p.send(ctx, t, actor, http.MethodPut, u, body, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(snapshotBudget)
	for {
		var now raw
		there, err := p.exists(ctx, t, actor, u, &now)
		switch {
		case err == nil && there && strings.EqualFold(now.Properties.ProvisioningState, "Succeeded"):
			return nil
		case err == nil && there && strings.EqualFold(now.Properties.ProvisioningState, "Failed"):
			return fmt.Errorf("Azure could not take %s", name)
		case time.Now().After(deadline):
			return fmt.Errorf("%s was not finished in time; it may still finish, so look before asking again", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
