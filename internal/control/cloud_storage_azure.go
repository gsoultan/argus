package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/azure"
)

/*
Azure disks differ from the other clouds' in three ways the plans below
carry: a disk lives in a resource group, which a new one has to be given; a
zone is optional, and a disk without one attaches only to a VM without one;
and Azure picks the LUN a disk is attached at, so there is no device name
to choose.
*/

// azureGroup is the resource group named g as Argus has read it in the
// account, in Azure's own spelling: a new disk goes only into a group that
// already holds something Argus can see.
func (a *API) azureGroup(ctx context.Context, accountID, g string) (string, bool) {
	for _, k := range []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindSnapshot, cloud.KindNetwork} {
		items, err := a.store.CloudResources(ctx, k)
		if err != nil {
			return "", false
		}
		for _, it := range items {
			if have := it.Resource.Details["resourceGroup"]; it.AccountID == accountID && have != "" && strings.EqualFold(have, g) {
				return have, true
			}
		}
	}
	return "", false
}

// azureNamed reports whether the account already has a kind named name in
// resource group g. Azure matches both whatever their case.
func (a *API) azureNamed(ctx context.Context, accountID string, kind cloud.Kind, g, name string) bool {
	items, err := a.store.CloudResources(ctx, kind)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(items, func(it CloudItem) bool {
		return it.AccountID == accountID && strings.EqualFold(it.Resource.Name, name) && strings.EqualFold(it.Resource.Details["resourceGroup"], g)
	})
}

// azureZoneWord is a zone in a sentence, where a resource may have none.
func azureZoneWord(z string) string {
	if z == "" {
		return "no zone"
	}
	return z
}

// azureZoneRedundant reports whether a disk type is kept in every zone of
// its region at once, and so attaches to a VM in any of them.
func azureZoneRedundant(t string) bool { return strings.HasSuffix(t, "_ZRS") }

func (a *API) planAzureVolumeCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudVolumeCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a disk takes {name, group, zone, sizeGb, type}")
	}
	s.Name, s.Group = strings.TrimSpace(s.Name), strings.TrimSpace(s.Group)
	if !azure.ValidDiskName(s.Name) {
		return ch, http.StatusBadRequest, errors.New("name the disk: letters, digits, . _ and -, up to 80, starting with a letter or digit")
	}
	if s.Type == "" {
		s.Type = azure.DiskTypes[0]
	}
	if !slices.Contains(azure.DiskTypes, s.Type) {
		return ch, http.StatusBadRequest, errors.New("pick a disk type: " + strings.Join(azure.DiskTypes, ", "))
	}
	if s.IOPS != 0 || s.Throughput != 0 {
		return ch, http.StatusBadRequest, errors.New("Argus makes Azure disks with their type's own performance")
	}
	var inst cloud.Resource
	if s.AttachTo != "" {
		var err error
		if inst, err = a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.AttachTo); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if !liveInstance(inst) {
			return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
		}
		if s.Group == "" {
			s.Group = inst.Details["resourceGroup"]
		}
		if !azureZoneRedundant(s.Type) {
			if s.Zone == "" {
				s.Zone = inst.Zone
			}
			if s.Zone != inst.Zone {
				return ch, http.StatusBadRequest, fmt.Errorf("%s is in %s; make the disk there to attach it", inst.Name, azureZoneWord(inst.Zone))
			}
		}
	}
	if azureZoneRedundant(s.Type) && s.Zone != "" {
		return ch, http.StatusBadRequest, errors.New("a " + s.Type + " disk is kept in every zone of its region; leave the zone out")
	}
	if s.Zone != "" {
		if _, ok := azure.ZoneNumber(in.Region, s.Zone); !ok {
			return ch, http.StatusBadRequest, errors.New("pick one of " + in.Region + "'s zones, " + in.Region + "-1 to " + in.Region + "-3, or none")
		}
	}
	if s.Group == "" {
		return ch, http.StatusBadRequest, errors.New("pick the resource group to make the disk in")
	}
	g, ok := a.azureGroup(ctx, m.acct.ID, s.Group)
	if !ok {
		return ch, http.StatusBadRequest, errors.New("Argus has read nothing in a resource group named " + s.Group + "; pick one it lists")
	}
	s.Group = g
	if a.azureNamed(ctx, m.acct.ID, cloud.KindVolume, s.Group, s.Name) {
		return ch, http.StatusConflict, errors.New("resource group " + s.Group + " already has a disk named " + s.Name)
	}
	if s.Snapshot != "" {
		snap, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindSnapshot, s.Snapshot)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick a snapshot in " + in.Region)
		}
		if snap.State != "completed" {
			return ch, http.StatusConflict, errors.New(snap.Name + " is not finished yet")
		}
		gb, _ := volumeGB(snap)
		if s.SizeGB == 0 {
			s.SizeGB = gb
		}
		if s.SizeGB < gb {
			return ch, http.StatusBadRequest, fmt.Errorf("a disk from %s is at least %d GiB", snap.Name, gb)
		}
		s.SnapshotName = snap.Name
	}
	if s.SizeGB < azure.MinDiskGB || s.SizeGB > azure.MaxDiskGB {
		return ch, http.StatusBadRequest, fmt.Errorf("an Azure disk is %d to %d GiB", azure.MinDiskGB, azure.MaxDiskGB)
	}
	for k, v := range s.Tags {
		if why := azure.TagProblem(k, v); why != "" {
			return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a tag: %s", k, why)
		}
	}
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		ch.Environment = e
	}
	s.Device = ""
	where := ""
	if s.AttachTo != "" {
		s.AttachName = inst.Name
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		where = ", attached to " + inst.Name
	}
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	place := in.Region
	if s.Zone != "" {
		place = s.Zone
	}
	ch.Summary = fmt.Sprintf("Make the disk %s: %d GiB %s in %s, resource group %s", s.Name, s.SizeGB, s.Type, place, s.Group)
	if s.SnapshotName != "" {
		ch.Summary += ", from " + s.SnapshotName
	}
	ch.Summary += where
	return ch, http.StatusOK, nil
}

// planAzureDiskGrow plans an Azure disk grow: its size, upwards. Azure
// grows a data disk while its VM runs, and the disk a VM starts from only
// while that VM is stopped.
func (a *API) planAzureDiskGrow(ctx context.Context, m cloudManaged, in *cloudChangeIn, res cloud.Resource, s cloudVolumeGrowSpec, ch CloudChange) (CloudChange, int, error) {
	s.FromType = res.Details["type"]
	if (s.Type != "" && s.Type != s.FromType) || s.IOPS != 0 || s.Throughput != 0 {
		return ch, http.StatusBadRequest, errors.New("Argus grows an Azure disk's size only; change its type or performance in the Azure portal")
	}
	s.Type = ""
	if s.SizeGB <= s.FromGB {
		return ch, http.StatusBadRequest, fmt.Errorf("Azure only grows a disk; pick more than %d GiB for %s", s.FromGB, res.Name)
	}
	if s.SizeGB > azure.MaxDiskGB {
		return ch, http.StatusBadRequest, fmt.Errorf("an Azure disk is at most %d GiB", azure.MaxDiskGB)
	}
	if res.State != "in-use" && res.State != "available" {
		return ch, http.StatusConflict, errors.New(res.Name + " is " + res.State + "; wait until it is ready")
	}
	if at, ok := attachedTo(res); ok {
		if inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, at.ID); err == nil {
			ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
			if res.Details["platform"] != "" && inst.State != "stopped" {
				return ch, http.StatusConflict, errors.New(res.Name + " is the disk " + inst.Name + " starts from; Azure grows that one only while the VM is stopped. Stop " + inst.Name + " first")
			}
		}
	}
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Grow %s from %d to %d GiB", res.Name, s.FromGB, s.SizeGB)
	return ch, http.StatusOK, nil
}
