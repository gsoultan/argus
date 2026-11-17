package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/gcp"
)

/*
Cloud, phase 3: disks, snapshots, images and Elastic IPs.

They go through the same path as instance changes (cloud_change_routes.go):
requested, approved where the environment asks for it, applied as the person
who asked, two audit rows at least. Levels: snapshotting a disk is operate;
making, growing, attaching and detaching disks, making images and allocating
or moving addresses are change; deleting a disk, snapshot or image and
releasing an address are remove, and need the resource's name typed.

A change that touches two resources (attaching a disk to an instance) is in
the stricter of their two environments.

An account connected before this phase has a manage role without these
permissions. The sync reads the role's policy (cloud_sync.go); a change it
cannot make is refused here with what to do, not sent to AWS to fail.
*/

// Specs, as stored on the change.

type cloudVolumeCreateSpec struct {
	cloud.VolumeSpec
	SnapshotName string `json:"snapshotName,omitempty"`
	AttachName   string `json:"attachName,omitempty"`
}

type cloudVolumeGrowSpec struct {
	cloud.VolumeChange
	FromGB   int32  `json:"fromGb"`
	FromType string `json:"fromType"`
}

type cloudAttachSpec struct {
	Instance     string `json:"instance"`
	InstanceName string `json:"instanceName,omitempty"`
	Device       string `json:"device,omitempty"`
}

type cloudDeleteSpec struct {
	FinalSnapshot bool `json:"finalSnapshot,omitempty"`
	WithSnapshots bool `json:"withSnapshots,omitempty"`
}

type cloudNameSpec struct {
	Name   string `json:"name"`
	Reboot bool   `json:"reboot,omitempty"`
}

var envRank = map[string]int{"development": 0, "staging": 1, "production": 2}

// stricterEnv is the stricter of two environments.
func stricterEnv(a, b string) string {
	if envRank[b] > envRank[a] {
		return b
	}
	return a
}

// volumeGB reads a disk's size and type from its inventory size, "100 GiB gp3".
func volumeGB(r cloud.Resource) (int32, string) {
	var gb int32
	var t string
	_, _ = fmt.Sscanf(r.Size, "%d GiB %s", &gb, &t)
	return gb, t
}

func attachedTo(r cloud.Resource) (cloud.Ref, bool) {
	for _, l := range r.Links {
		if l.Kind == cloud.KindInstance {
			return l, true
		}
	}
	return cloud.Ref{}, false
}

func liveInstance(r cloud.Resource) bool {
	return r.State != "terminated" && r.State != "shutting-down"
}

// cloudKindWord is how a kind reads in a sentence.
var cloudKindWord = map[cloud.Kind]string{
	cloud.KindInstance: "instance", cloud.KindVolume: "disk", cloud.KindSnapshot: "snapshot",
	cloud.KindImage: "image", cloud.KindAddress: "Elastic IP", cloud.KindFirewall: "security group",
	cloud.KindTargetGroup: "target group", cloud.KindLoadBalancer: "load balancer",
}

// storageMissing refuses an action the manage role lacks the permissions
// for, saying how to add them.
func (a *API) storageMissing(m cloudManaged, act cloud.Action) error {
	pc, ok := m.provider.(cloud.PolicyChecker)
	if !ok {
		return nil
	}
	return a.missingFor(m, pc.Needs(act))
}

// planStorage checks a storage request and works out what it will do.
func (a *API) planStorage(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.Storage); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot change disks yet")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ctx := r.Context()
	ch.Environment = m.acct.Environment

	// A create has nothing to read yet.
	switch in.Action {
	case cloud.ActionVolumeCreate:
		if m.acct.Provider == "gcp" {
			return a.planGCPVolumeCreate(ctx, m, in, ch)
		}
		return a.planVolumeCreate(ctx, m, in, ch)
	case cloud.ActionAddressAllocate:
		var s cloudNameSpec
		if len(in.Spec) > 0 {
			if err := json.Unmarshal(in.Spec, &s); err != nil {
				return ch, http.StatusBadRequest, errors.New("an address takes {name}")
			}
		}
		s.Name = strings.TrimSpace(s.Name)
		if m.acct.Provider == "gcp" {
			if in.Region == "" || in.Region == gcp.GlobalRegion {
				return ch, http.StatusBadRequest, errors.New("pick the region of the VMs the address is for")
			}
			if !gcp.ValidName(s.Name) {
				return ch, http.StatusBadRequest, errors.New("name the address: lowercase letters, digits and -, up to 63, starting with a letter")
			}
			for _, e := range a.regionItems(ctx, m.acct.ID, in.Region, cloud.KindAddress) {
				if e.Name == s.Name {
					return ch, http.StatusConflict, errors.New("there is already an address " + s.Name + " in " + in.Region)
				}
			}
			ch.Spec, _ = json.Marshal(s)
			ch.ResourceName = s.Name
			ch.Summary = "Reserve the static address " + s.Name + " in " + in.Region
			return ch, http.StatusOK, nil
		}
		if len(s.Name) > 128 {
			return ch, http.StatusBadRequest, errors.New("keep the name under 128 characters")
		}
		ch.Spec, _ = json.Marshal(s)
		ch.ResourceName = s.Name
		ch.Summary = "Allocate an Elastic IP in " + in.Region
		if s.Name != "" {
			ch.Summary += " named " + s.Name
		}
		return ch, http.StatusOK, nil
	}

	kind := cloud.KindOf(in.Action)
	res, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, kind, in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = res.ID, res.Name
	ch.Environment = cloudEnvOf(res.Tags, m.acct.Environment)
	stack := res.Tags["aws:cloudformation:stack-name"]
	gcpAcct := m.acct.Provider == "gcp"

	switch in.Action {
	case cloud.ActionVolumeGrow:
		var s cloudVolumeGrowSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("a disk change takes {sizeGb, type, iops, throughput}")
		}
		s.FromGB, s.FromType = volumeGB(res)
		if m.acct.Provider == "gcp" {
			return planGCPDiskGrow(res, s, ch)
		}
		if s.Type == s.FromType {
			s.Type = ""
		}
		if s.SizeGB == s.FromGB {
			s.SizeGB = 0
		}
		if s.SizeGB == 0 && s.Type == "" && s.IOPS == 0 && s.Throughput == 0 {
			return ch, http.StatusBadRequest, errors.New("nothing to change")
		}
		if s.SizeGB != 0 && s.SizeGB < s.FromGB {
			return ch, http.StatusBadRequest, fmt.Errorf("AWS cannot shrink a disk; %s is %d GiB", res.Name, s.FromGB)
		}
		t := s.FromType
		if s.Type != "" {
			if !cloud.ValidVolumeType(s.Type) {
				return ch, http.StatusBadRequest, errors.New("pick a disk type: " + strings.Join(cloud.VolumeTypes, ", "))
			}
			t = s.Type
		}
		size := max(s.SizeGB, s.FromGB)
		if lo, hi := cloud.VolumeLimits(t); size < lo || size > hi {
			return ch, http.StatusBadRequest, fmt.Errorf("a %s disk is %d to %d GiB", t, lo, hi)
		}
		if err := checkPerformance(t, s.IOPS, s.Throughput); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if res.State != "in-use" && res.State != "available" {
			return ch, http.StatusConflict, errors.New(res.Name + " is " + res.State + "; wait until it is ready")
		}
		ch.Spec, _ = json.Marshal(s)
		var parts []string
		if s.SizeGB != 0 {
			parts = append(parts, fmt.Sprintf("from %d to %d GiB", s.FromGB, s.SizeGB))
		}
		if s.Type != "" {
			parts = append(parts, "from "+s.FromType+" to "+s.Type)
		}
		if s.IOPS != 0 {
			parts = append(parts, fmt.Sprintf("to %d IOPS", s.IOPS))
		}
		if s.Throughput != 0 {
			parts = append(parts, fmt.Sprintf("to %d MB/s", s.Throughput))
		}
		verb := "Change "
		if s.SizeGB != 0 {
			verb = "Grow "
		}
		ch.Summary = verb + res.Name + " " + strings.Join(parts, ", ")

	case cloud.ActionVolumeAttach:
		var s cloudAttachSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil || s.Instance == "" {
			return ch, http.StatusBadRequest, errors.New("pick the instance to attach it to")
		}
		if res.State != "available" {
			return ch, http.StatusConflict, errors.New(res.Name + " is already attached; detach it first")
		}
		inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.Instance)
		if err != nil {
			return ch, http.StatusBadRequest, err
		}
		if !liveInstance(inst) {
			return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
		}
		if inst.Zone != res.Zone {
			return ch, http.StatusBadRequest, fmt.Errorf("%s is in %s and %s in %s; a disk attaches only in its own zone", res.Name, res.Zone, inst.Name, inst.Zone)
		}
		var used []string
		for _, l := range inst.Links {
			if l.Kind == cloud.KindVolume {
				used = append(used, l.Name)
			}
		}
		if m.acct.Provider == "gcp" {
			// Google names the device after the disk, as its console does:
			// the server sees /dev/disk/by-id/google-<name>.
			s.Device = res.Name
			if slices.Contains(used, s.Device) {
				return ch, http.StatusConflict, errors.New(inst.Name + " already has a disk attached as " + s.Device)
			}
		} else if s.Device == "" {
			s.Device = cloud.FreeDevice(used)
			if s.Device == "" {
				return ch, http.StatusConflict, errors.New(inst.Name + " has no free device name left")
			}
		} else if !cloud.ValidDevice(s.Device) || slices.Contains(used, s.Device) {
			return ch, http.StatusBadRequest, errors.New("pick a free device from /dev/sdf to /dev/sdp")
		}
		s.InstanceName = inst.Name
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Attach " + res.Name + " to " + inst.Name + " as " + s.Device

	case cloud.ActionVolumeDetach:
		at, ok := attachedTo(res)
		if !ok {
			return ch, http.StatusConflict, errors.New(res.Name + " is not attached")
		}
		s := cloudAttachSpec{Instance: at.ID, Device: at.Name}
		if inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, at.ID); err == nil {
			s.InstanceName = inst.Name
			ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
			if root := inst.Details["rootDeviceName"]; root != "" && root == at.Name && inst.State != "stopped" {
				return ch, http.StatusConflict, errors.New(res.Name + " is " + inst.Name + "'s root disk; stop the instance first")
			}
		}
		if s.InstanceName == "" {
			s.InstanceName = at.ID
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Detach " + res.Name + " (" + at.Name + ") from " + s.InstanceName

	case cloud.ActionVolumeDelete:
		var s cloudDeleteSpec
		if len(in.Spec) > 0 {
			if err := json.Unmarshal(in.Spec, &s); err != nil {
				return ch, http.StatusBadRequest, errors.New("deleting a disk takes {finalSnapshot}")
			}
		}
		if err := confirmTyped(in, res); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if res.State != "available" {
			return ch, http.StatusConflict, errors.New(res.Name + " is attached; detach it first, which is its own change")
		}
		if s.FinalSnapshot && m.acct.Provider == "gcp" {
			return ch, http.StatusBadRequest, errors.New("in Google Cloud a final snapshot is its own change: take a snapshot of " + res.Name + " first, then delete it")
		}
		if stack != "" {
			return ch, http.StatusConflict, errors.New("Argus will not delete " + res.Name + ": it belongs to CloudFormation stack " + stack)
		}
		ch.Spec, _ = json.Marshal(cloudDeleteSpec{FinalSnapshot: s.FinalSnapshot})
		ch.Summary = fmt.Sprintf("Delete the disk %s (%s, %s)", res.Name, res.ID, res.Size)
		if s.FinalSnapshot {
			ch.Summary += ", after a final snapshot"
		}

	case cloud.ActionSnapshotCreate:
		var s cloudNameSpec
		if len(in.Spec) > 0 {
			if err := json.Unmarshal(in.Spec, &s); err != nil {
				return ch, http.StatusBadRequest, errors.New("a snapshot takes {name}")
			}
		}
		s.Name = strings.TrimSpace(s.Name)
		if m.acct.Provider == "gcp" {
			if s.Name == "" {
				s.Name = gcpSnapshotName(res.Name, time.Now().UTC())
			}
			if !gcp.ValidName(s.Name) {
				return ch, http.StatusBadRequest, errors.New("a snapshot's name is lowercase letters, digits and -, up to 63, starting with a letter")
			}
		}
		if s.Name == "" {
			s.Name = res.Name + " " + time.Now().UTC().Format("2006-01-02 15:04")
		}
		if len(s.Name) > 128 {
			return ch, http.StatusBadRequest, errors.New("keep the name under 128 characters")
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Snapshot " + res.Name + " as \"" + s.Name + "\""

	case cloud.ActionSnapshotDelete:
		if err := confirmTyped(in, res); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if img, ok := a.imageUsing(ctx, m.acct.ID, in.Region, res.ID); ok {
			return ch, http.StatusConflict, errors.New("the image " + img + " is made from " + res.Name + "; delete the image first, or delete it with its snapshots")
		}
		ch.Spec = json.RawMessage("{}")
		ch.Summary = fmt.Sprintf("Delete the snapshot %s (%s, %s)", res.Name, res.ID, res.Size)

	case cloud.ActionImageCreate:
		var s cloudNameSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("an image takes {name, reboot}")
		}
		s.Name = strings.TrimSpace(s.Name)
		if !liveInstance(res) {
			return ch, http.StatusConflict, errors.New(res.Name + " is being removed")
		}
		if gcpAcct {
			// Images are global in Google Cloud, and made of the boot disk.
			if !gcp.ValidName(s.Name) {
				return ch, http.StatusBadRequest, errors.New("an image's name is lowercase letters, digits and -, up to 63, starting with a letter")
			}
			if s.Reboot {
				return ch, http.StatusBadRequest, errors.New("Google does not reboot a VM for an image; stop it first for a consistent copy")
			}
			if a.imageNamed(ctx, m.acct.ID, gcp.GlobalRegion, s.Name) {
				return ch, http.StatusConflict, errors.New("there is already an image named " + s.Name + " in this project")
			}
			ch.Spec, _ = json.Marshal(s)
			ch.Summary = "Make the image \"" + s.Name + "\" from the boot disk of " + res.Name
			if res.State == "running" {
				ch.Summary += ", copied while it runs"
			}
			break
		}
		if !cloud.ValidImageName(s.Name) {
			return ch, http.StatusBadRequest, errors.New("an image name is 3 to 128 letters, digits, spaces and ( ) [ ] . / ' @ _ -")
		}
		if a.imageNamed(ctx, m.acct.ID, in.Region, s.Name) {
			return ch, http.StatusConflict, errors.New("there is already an image named " + s.Name + " in " + in.Region)
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Make the image \"" + s.Name + "\" from " + res.Name
		if s.Reboot && res.State == "running" {
			ch.Summary += ", rebooting it for a consistent copy"
		}

	case cloud.ActionImageDelete:
		var s cloudDeleteSpec
		if len(in.Spec) > 0 {
			if err := json.Unmarshal(in.Spec, &s); err != nil {
				return ch, http.StatusBadRequest, errors.New("deleting an image takes {withSnapshots}")
			}
		}
		if err := confirmTyped(in, res); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if gcpAcct && s.WithSnapshots {
			return ch, http.StatusBadRequest, errors.New("a Google image has no snapshots of its own to delete with it")
		}
		ch.Spec, _ = json.Marshal(cloudDeleteSpec{WithSnapshots: s.WithSnapshots})
		ch.Summary = "Delete the image " + res.Name + " (" + res.ID + ")"
		if s.WithSnapshots {
			ch.Summary += " and its snapshots"
		}
		if n := a.launchedFrom(ctx, m.acct.ID, in.Region, res.ID); n > 0 {
			ch.Summary += fmt.Sprintf("; the %d instance%s launched from it keep running", n, plural(n))
		}

	case cloud.ActionAddressAssociate:
		var s cloudAttachSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil || s.Instance == "" {
			return ch, http.StatusBadRequest, errors.New("pick the instance to attach it to")
		}
		if res.State == "attached" {
			return ch, http.StatusConflict, errors.New(res.PublicIP + " is attached to something; take it off first")
		}
		if gcpAcct && (res.Region == gcp.GlobalRegion || res.PublicIP == "") {
			return ch, http.StatusBadRequest, errors.New(res.Name + " is not an external regional address; a VM takes only those")
		}
		inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.Instance)
		if err != nil {
			return ch, http.StatusBadRequest, err
		}
		if !liveInstance(inst) {
			return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
		}
		if ip, ok := a.addressOf(ctx, m.acct.ID, in.Region, inst.ID); ok {
			return ch, http.StatusConflict, errors.New(inst.Name + " already has the " + addressWord(gcpAcct) + " " + ip + "; take that one off first")
		}
		s.InstanceName = inst.Name
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		ch.Spec, _ = json.Marshal(s)
		if gcpAcct {
			ch.Summary = "Assign the static address " + res.PublicIP + " to " + inst.Name
		} else {
			ch.Summary = "Attach the Elastic IP " + res.PublicIP + " to " + inst.Name
		}
		if inst.PublicIP != "" {
			ch.Summary += ", replacing its public address " + inst.PublicIP
		}

	case cloud.ActionAddressDisassociate:
		if res.State != "attached" {
			return ch, http.StatusConflict, errors.New(res.PublicIP + " is not attached to anything")
		}
		s := cloudAttachSpec{}
		if at, ok := attachedTo(res); ok {
			s.Instance, s.InstanceName = at.ID, at.ID
			if inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, at.ID); err == nil {
				s.InstanceName = inst.Name
				ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
			}
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Take the " + addressWord(gcpAcct) + " " + res.PublicIP + " off"
		if s.InstanceName != "" {
			ch.Summary += " " + s.InstanceName
		}
		if gcpAcct {
			ch.Summary += "; it has no public address until it is given one"
		}

	case cloud.ActionAddressRelease:
		if strings.TrimSpace(in.Confirm) != res.PublicIP && strings.TrimSpace(in.Confirm) != res.Name {
			return ch, http.StatusBadRequest, errors.New("type the address, " + res.PublicIP + ", to release it")
		}
		if res.State == "attached" {
			return ch, http.StatusConflict, errors.New(res.PublicIP + " is attached; take it off first, which is its own change")
		}
		ch.Spec = json.RawMessage("{}")
		if gcpAcct {
			ip := res.PublicIP
			if ip == "" && len(res.Addresses) > 0 {
				ip = res.Addresses[0]
			}
			ch.Summary = "Release the static address " + res.Name + " (" + ip + ") back to Google; the address is gone for good"
		} else {
			ch.Summary = "Release the Elastic IP " + res.PublicIP + " back to Amazon; the address is gone for good"
		}
	}
	if stack != "" && ch.Level != cloud.LevelOperate && in.Action != cloud.ActionVolumeDelete {
		ch.Summary += " (it belongs to CloudFormation stack " + stack + ", which may change it back)"
	}
	return ch, http.StatusOK, nil
}

func confirmTyped(in *cloudChangeIn, res cloud.Resource) error {
	if t := strings.TrimSpace(in.Confirm); t != res.Name && t != res.ID {
		return fmt.Errorf("type the %s's name, %s, to delete it", cloudKindWord[res.Kind], res.Name)
	}
	return nil
}

func checkPerformance(t string, iops, throughput int32) error {
	switch t {
	case "gp3":
		if iops != 0 && (iops < 3000 || iops > 16000) {
			return errors.New("a gp3 disk has 3000 to 16000 IOPS")
		}
		if throughput != 0 && (throughput < 125 || throughput > 1000) {
			return errors.New("a gp3 disk has 125 to 1000 MB/s")
		}
	case "io1", "io2":
		if iops != 0 && (iops < 100 || iops > 64000) {
			return errors.New("an " + t + " disk has 100 to 64000 IOPS")
		}
		if throughput != 0 {
			return errors.New("only gp3 disks take a throughput")
		}
	default:
		if iops != 0 || throughput != 0 {
			return errors.New("a " + t + " disk takes no IOPS or throughput; only gp3, io1 and io2 do")
		}
	}
	return nil
}

func (a *API) planVolumeCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudVolumeCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a disk takes {name, zone, sizeGb, type}")
	}
	s.Name = strings.TrimSpace(s.Name)
	if len(s.Name) > 128 {
		return ch, http.StatusBadRequest, errors.New("keep the name under 128 characters")
	}
	if s.Type == "" {
		s.Type = "gp3"
	}
	if !cloud.ValidVolumeType(s.Type) {
		return ch, http.StatusBadRequest, errors.New("pick a disk type: " + strings.Join(cloud.VolumeTypes, ", "))
	}
	if !a.zoneIn(ctx, m.acct.ID, in.Region, s.Zone) {
		return ch, http.StatusBadRequest, errors.New("pick one of " + in.Region + "'s zones")
	}
	if (s.Type == "io1" || s.Type == "io2") && s.IOPS == 0 {
		return ch, http.StatusBadRequest, errors.New("an " + s.Type + " disk needs its IOPS")
	}
	if err := checkPerformance(s.Type, s.IOPS, s.Throughput); err != nil {
		return ch, http.StatusBadRequest, err
	}
	if s.Snapshot != "" {
		snap, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindSnapshot, s.Snapshot)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick a snapshot in " + in.Region)
		}
		if snap.State != "completed" {
			return ch, http.StatusConflict, errors.New(snap.Name + " is not finished yet")
		}
		var gb int32
		_, _ = fmt.Sscanf(snap.Size, "%d GiB", &gb)
		if s.SizeGB == 0 {
			s.SizeGB = gb
		}
		if s.SizeGB < gb {
			return ch, http.StatusBadRequest, fmt.Errorf("a disk from %s is at least %d GiB", snap.Name, gb)
		}
		s.SnapshotName = snap.Name
	}
	if lo, hi := cloud.VolumeLimits(s.Type); s.SizeGB < lo || s.SizeGB > hi {
		return ch, http.StatusBadRequest, fmt.Errorf("a %s disk is %d to %d GiB", s.Type, lo, hi)
	}
	for k, v := range s.Tags {
		if !cloud.ValidTagKey(k) || len(v) > 256 || k == "Name" {
			return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a tag here", k)
		}
	}
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		ch.Environment = e
	}
	where := ""
	if s.AttachTo != "" {
		inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.AttachTo)
		if err != nil {
			return ch, http.StatusBadRequest, err
		}
		if !liveInstance(inst) {
			return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
		}
		if inst.Zone != s.Zone {
			return ch, http.StatusBadRequest, fmt.Errorf("%s is in %s; make the disk there to attach it", inst.Name, inst.Zone)
		}
		var used []string
		for _, l := range inst.Links {
			if l.Kind == cloud.KindVolume {
				used = append(used, l.Name)
			}
		}
		if s.Device == "" {
			s.Device = cloud.FreeDevice(used)
		}
		if !cloud.ValidDevice(s.Device) || slices.Contains(used, s.Device) {
			return ch, http.StatusBadRequest, errors.New("pick a free device from /dev/sdf to /dev/sdp")
		}
		s.AttachName = inst.Name
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		where = ", attached to " + inst.Name + " as " + s.Device
	} else {
		s.Device = ""
	}
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	what := "a disk"
	if s.Name != "" {
		what = "the disk " + s.Name
	}
	ch.Summary = fmt.Sprintf("Make %s: %d GiB %s, encrypted, in %s", what, s.SizeGB, s.Type, s.Zone)
	if s.SnapshotName != "" {
		ch.Summary += ", from " + s.SnapshotName
	}
	ch.Summary += where
	return ch, http.StatusOK, nil
}

// zoneIn reports whether zone is one Argus has seen in the account's region.
func (a *API) zoneIn(ctx context.Context, accountID, region, zone string) bool {
	if zone == "" || !strings.HasPrefix(zone, region) {
		return false
	}
	for _, k := range []cloud.Kind{cloud.KindSubnet, cloud.KindInstance, cloud.KindVolume} {
		items, err := a.store.CloudResources(ctx, k)
		if err != nil {
			return false
		}
		for _, it := range items {
			if it.AccountID == accountID && it.Resource.Region == region && it.Resource.Zone == zone {
				return true
			}
		}
	}
	return false
}

func (a *API) regionItems(ctx context.Context, accountID, region string, kind cloud.Kind) []cloud.Resource {
	items, err := a.store.CloudResources(ctx, kind)
	if err != nil {
		return nil
	}
	var out []cloud.Resource
	for _, it := range items {
		if it.AccountID == accountID && it.Resource.Region == region {
			out = append(out, it.Resource)
		}
	}
	return out
}

func (a *API) imageUsing(ctx context.Context, accountID, region, snapshot string) (string, bool) {
	for _, im := range a.regionItems(ctx, accountID, region, cloud.KindImage) {
		for _, l := range im.Links {
			if l.Kind == cloud.KindSnapshot && l.ID == snapshot {
				return im.Name, true
			}
		}
	}
	return "", false
}

func (a *API) imageNamed(ctx context.Context, accountID, region, name string) bool {
	for _, im := range a.regionItems(ctx, accountID, region, cloud.KindImage) {
		if im.Name == name {
			return true
		}
	}
	return false
}

func (a *API) launchedFrom(ctx context.Context, accountID, region, image string) int {
	n := 0
	for _, i := range a.regionItems(ctx, accountID, region, cloud.KindInstance) {
		if i.Details["image"] == image && liveInstance(i) {
			n++
		}
	}
	return n
}

// addressWord is what a provider calls an address a VM keeps.
func addressWord(gcpAcct bool) string {
	if gcpAcct {
		return "static address"
	}
	return "Elastic IP"
}

func (a *API) addressOf(ctx context.Context, accountID, region, instance string) (string, bool) {
	for _, e := range a.regionItems(ctx, accountID, region, cloud.KindAddress) {
		if at, ok := attachedTo(e); ok && at.ID == instance {
			return e.PublicIP, true
		}
	}
	return "", false
}

// applyStorage runs a storage change as the person who asked.
func (a *API) applyStorage(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	st, ok := m.manager.(cloud.Storage)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot change disks yet")
	}
	actor := cloud.Actor(ch.RequestedBy)
	t, region, id := m.target, ch.Region, ch.ResourceID
	switch ch.Action {
	case cloud.ActionVolumeCreate:
		var s cloudVolumeCreateSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		if s.Tags == nil {
			s.Tags = map[string]string{}
		}
		s.Tags["argus:change"] = ch.ID
		res, err := st.CreateVolume(ctx, t, actor, region, s.VolumeSpec)
		if res.ResourceID != "" && res.Detail == "" {
			res.Detail = "Made " + res.ResourceID
		}
		return res, err
	case cloud.ActionVolumeGrow:
		var s cloudVolumeGrowSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		res, err := st.ModifyVolume(ctx, t, actor, region, id, s.VolumeChange)
		if err == nil && s.SizeGB > s.FromGB {
			res.Detail = "AWS grows the disk in the background. The file system on it still has its old size until it is grown too: " +
				"from the server's Disks tab when it has the Argus agent"
			if m.acct.Provider == "gcp" {
				res.Detail = "Google grows the disk in a few seconds. The file system on it still has its old size until it is grown too: " +
					"from the server's Disks tab when it has the Argus agent"
			}
		}
		return res, err
	case cloud.ActionVolumeAttach:
		var s cloudAttachSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		return st.AttachVolume(ctx, t, actor, region, id, s.Instance, s.Device)
	case cloud.ActionVolumeDetach:
		return st.DetachVolume(ctx, t, actor, region, id)
	case cloud.ActionVolumeDelete:
		var s cloudDeleteSpec
		_ = json.Unmarshal(ch.Spec, &s)
		return st.DeleteVolume(ctx, t, actor, region, id, s.FinalSnapshot)
	case cloud.ActionSnapshotCreate:
		var s cloudNameSpec
		_ = json.Unmarshal(ch.Spec, &s)
		res, err := st.CreateSnapshot(ctx, t, actor, region, id, s.Name)
		if err == nil {
			res.Detail = "Snapshot " + res.ResourceID + " finishes in the background"
			// The change is about the disk; the snapshot is in the detail.
			res.ResourceID = id
		}
		return res, err
	case cloud.ActionSnapshotDelete:
		return st.DeleteSnapshot(ctx, t, actor, region, id)
	case cloud.ActionImageCreate:
		var s cloudNameSpec
		_ = json.Unmarshal(ch.Spec, &s)
		res, err := st.CreateImage(ctx, t, actor, region, id, s.Name, s.Reboot)
		if err == nil {
			res.Detail = "Image " + res.ResourceID + " finishes in the background"
			res.ResourceID = id
		}
		return res, err
	case cloud.ActionImageDelete:
		var s cloudDeleteSpec
		_ = json.Unmarshal(ch.Spec, &s)
		return st.DeleteImage(ctx, t, actor, region, id, s.WithSnapshots)
	case cloud.ActionAddressAllocate:
		var s cloudNameSpec
		_ = json.Unmarshal(ch.Spec, &s)
		return st.AllocateAddress(ctx, t, actor, region, s.Name)
	case cloud.ActionAddressAssociate:
		var s cloudAttachSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		return st.AssociateAddress(ctx, t, actor, region, id, s.Instance)
	case cloud.ActionAddressDisassociate:
		return st.DisassociateAddress(ctx, t, actor, region, id)
	case cloud.ActionAddressRelease:
		return st.ReleaseAddress(ctx, t, actor, region, id)
	}
	return cloud.Result{}, fmt.Errorf("%q is not a storage change", ch.Action)
}

// gcpDiskMaxGB is the largest a Compute Engine persistent disk grows to.
const gcpDiskMaxGB = 65536

// planGCPDiskGrow plans a Google Cloud disk grow: Google resizes a disk
// up, and changes neither its type nor its performance that way.
func planGCPDiskGrow(res cloud.Resource, s cloudVolumeGrowSpec, ch CloudChange) (CloudChange, int, error) {
	if (s.Type != "" && s.Type != s.FromType) || s.IOPS != 0 || s.Throughput != 0 {
		return ch, http.StatusBadRequest, errors.New("Argus grows a Google Cloud disk's size only; change its type or performance in the Google Cloud console")
	}
	s.Type = ""
	if s.SizeGB <= s.FromGB {
		return ch, http.StatusBadRequest, fmt.Errorf("Google Cloud only grows a disk; pick more than %d GB for %s", s.FromGB, res.Name)
	}
	if s.SizeGB > gcpDiskMaxGB {
		return ch, http.StatusBadRequest, fmt.Errorf("a Google Cloud disk is at most %d GB", gcpDiskMaxGB)
	}
	if res.State != "in-use" && res.State != "available" {
		return ch, http.StatusConflict, errors.New(res.Name + " is " + res.State + "; wait until it is ready")
	}
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Grow %s from %d to %d GB", res.Name, s.FromGB, s.SizeGB)
	return ch, http.StatusOK, nil
}

// gcpSnapshotName is a snapshot's name when none is given: the disk's
// name and the time, as a Compute Engine name.
func gcpSnapshotName(disk string, at time.Time) string {
	stamp := at.Format("20060102-1504")
	base := strings.Trim(reNotGCPName.ReplaceAllString(strings.ToLower(disk), "-"), "-")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "disk-" + base
	}
	if len(base) > 63-len(stamp)-1 {
		base = strings.TrimRight(base[:63-len(stamp)-1], "-")
	}
	return base + "-" + stamp
}

var reNotGCPName = regexp.MustCompile(`[^a-z0-9-]+`)

// planGCPVolumeCreate plans a new Google Cloud disk: named, as Google
// requires, in one of the region's zones, empty or from a snapshot, and
// attached when asked to.
func (a *API) planGCPVolumeCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudVolumeCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a disk takes {name, zone, sizeGb, type}")
	}
	s.Name = strings.TrimSpace(s.Name)
	if !gcp.ValidName(s.Name) {
		return ch, http.StatusBadRequest, errors.New("name the disk: lowercase letters, digits and -, up to 63, starting with a letter")
	}
	if s.Type == "" {
		s.Type = "pd-balanced"
	}
	if !slices.Contains(gcp.DiskTypes, s.Type) {
		return ch, http.StatusBadRequest, errors.New("pick a disk type: " + strings.Join(gcp.DiskTypes, ", "))
	}
	if s.IOPS != 0 || s.Throughput != 0 {
		return ch, http.StatusBadRequest, errors.New("Argus makes Google Cloud disks with their type's own performance")
	}
	if !a.zoneIn(ctx, m.acct.ID, in.Region, s.Zone) {
		return ch, http.StatusBadRequest, errors.New("pick one of " + in.Region + "'s zones")
	}
	for _, it := range a.regionItems(ctx, m.acct.ID, in.Region, cloud.KindVolume) {
		if it.Name == s.Name && it.Zone == s.Zone {
			return ch, http.StatusConflict, errors.New(s.Zone + " already has a disk named " + s.Name)
		}
	}
	if s.Snapshot != "" {
		snap, err := a.store.CloudResourceOf(ctx, m.acct.ID, gcp.GlobalRegion, cloud.KindSnapshot, s.Snapshot)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick one of the project's snapshots")
		}
		if snap.State != "ready" {
			return ch, http.StatusConflict, errors.New(snap.Name + " is not ready yet")
		}
		gb, _ := volumeGB(snap)
		if s.SizeGB == 0 {
			s.SizeGB = gb
		}
		if s.SizeGB < gb {
			return ch, http.StatusBadRequest, fmt.Errorf("a disk from %s is at least %d GB", snap.Name, gb)
		}
		s.SnapshotName = snap.Name
	}
	if s.SizeGB < gcp.MinDiskGB || s.SizeGB > gcp.MaxDiskGB {
		return ch, http.StatusBadRequest, fmt.Errorf("a Google Cloud disk is %d to %d GB", gcp.MinDiskGB, gcp.MaxDiskGB)
	}
	for k, v := range s.Tags {
		if why := gcp.LabelProblem(k, v); why != "" {
			return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a label: %s", k, why)
		}
	}
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		ch.Environment = e
	}
	where := ""
	if s.AttachTo != "" {
		inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.AttachTo)
		if err != nil {
			return ch, http.StatusBadRequest, err
		}
		if !liveInstance(inst) {
			return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
		}
		if inst.Zone != s.Zone {
			return ch, http.StatusBadRequest, fmt.Errorf("%s is in %s; make the disk there to attach it", inst.Name, inst.Zone)
		}
		s.Device = s.Name
		for _, l := range inst.Links {
			if l.Kind == cloud.KindVolume && l.Name == s.Device {
				return ch, http.StatusConflict, errors.New(inst.Name + " already has a disk attached as " + s.Device)
			}
		}
		s.AttachName = inst.Name
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		where = ", attached to " + inst.Name
	} else {
		s.Device = ""
	}
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Make the disk %s: %d GB %s in %s", s.Name, s.SizeGB, s.Type, s.Zone)
	if s.SnapshotName != "" {
		ch.Summary += ", from " + s.SnapshotName
	}
	ch.Summary += where
	return ch, http.StatusOK, nil
}
