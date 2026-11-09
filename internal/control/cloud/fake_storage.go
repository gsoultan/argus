package cloud

import (
	"context"
	"errors"
	"fmt"
)

// The fake as Storage: each call edits Resources the way AWS would, so the
// next Inventory sees it, and refuses what AWS refuses.

func (f *Fake) storageCall(a Account, act Action, what string, actor Actor) error {
	f.record(string(act) + " " + what + " " + string(actor))
	if err := f.admit(a); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.StorageErr[act]; e != nil {
		delete(f.StorageErr, act)
		return e
	}
	return nil
}

func (f *Fake) add(region string, r Resource) string {
	f.made++
	r.ID = fmt.Sprintf("%s-fake%04d", map[Kind]string{KindVolume: "vol", KindSnapshot: "snap", KindImage: "ami", KindAddress: "eipalloc"}[r.Kind], f.made)
	r.Region = region
	if r.Name == "" {
		r.Name = r.ID
	}
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	f.Resources[region] = append(f.Resources[region], r)
	return r.ID
}

func (f *Fake) drop(region string, i int) {
	f.Resources[region] = append(f.Resources[region][:i], f.Resources[region][i+1:]...)
}

func (f *Fake) link(region string, i int, ref Ref) {
	f.Resources[region][i].Links = append(f.Resources[region][i].Links, ref)
}

func (f *Fake) unlink(region string, i int, kind Kind, id string) {
	r := &f.Resources[region][i]
	out := r.Links[:0]
	for _, l := range r.Links {
		if !(l.Kind == kind && l.ID == id) {
			out = append(out, l)
		}
	}
	r.Links = out
}

func (f *Fake) CreateVolume(_ context.Context, a Account, actor Actor, region string, s VolumeSpec) (Result, error) {
	if err := f.storageCall(a, ActionVolumeCreate, s.Zone, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.add(region, Resource{Kind: KindVolume, Name: s.Name, Zone: s.Zone, State: "available",
		Size: fmt.Sprintf("%d GiB %s", s.SizeGB, s.Type), Details: map[string]string{"encrypted": "yes"}})
	r := Result{ResourceID: id, RequestID: f.request()}
	if s.AttachTo != "" {
		if err := f.attach(region, id, s.AttachTo, s.Device); err != nil {
			return r, fmt.Errorf("made %s, but attaching it to %s failed; attach it from Disks: %w", id, s.AttachTo, err)
		}
		r.Detail = "Attached to " + s.AttachTo + " as " + s.Device
	}
	return r, nil
}

func (f *Fake) attach(region, volume, instance, device string) error {
	v, err := f.findKind(region, KindVolume, volume)
	if err != nil {
		return err
	}
	if f.Resources[region][v].State != "available" {
		return errors.New("VolumeInUse: " + volume + " is already attached")
	}
	i, err := f.find(region, instance)
	if err != nil {
		return err
	}
	f.Resources[region][v].State = "in-use"
	f.link(region, v, Ref{Kind: KindInstance, ID: instance, Name: device})
	f.link(region, i, Ref{Kind: KindVolume, ID: volume, Name: device})
	return nil
}

func (f *Fake) ModifyVolume(_ context.Context, a Account, actor Actor, region, id string, c VolumeChange) (Result, error) {
	if err := f.storageCall(a, ActionVolumeGrow, id, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.findKind(region, KindVolume, id)
	if err != nil {
		return Result{}, err
	}
	if c.SizeGB > 0 {
		t := c.Type
		if t == "" {
			t = "gp3"
		}
		f.Resources[region][v].Size = fmt.Sprintf("%d GiB %s", c.SizeGB, t)
	}
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) AttachVolume(_ context.Context, a Account, actor Actor, region, volume, instance, device string) (Result, error) {
	if err := f.storageCall(a, ActionVolumeAttach, volume+" "+instance+" "+device, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.attach(region, volume, instance, device); err != nil {
		return Result{}, err
	}
	return Result{ResourceID: volume, RequestID: f.request()}, nil
}

func (f *Fake) DetachVolume(_ context.Context, a Account, actor Actor, region, volume string) (Result, error) {
	if err := f.storageCall(a, ActionVolumeDetach, volume, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.findKind(region, KindVolume, volume)
	if err != nil {
		return Result{}, err
	}
	for _, l := range f.Resources[region][v].Links {
		if l.Kind == KindInstance {
			if i, err := f.find(region, l.ID); err == nil {
				f.unlink(region, i, KindVolume, volume)
			}
		}
	}
	r := &f.Resources[region][v]
	r.Links = nil
	r.State = "available"
	return Result{ResourceID: volume, RequestID: f.request()}, nil
}

func (f *Fake) DeleteVolume(_ context.Context, a Account, actor Actor, region, volume string, finalSnapshot bool) (Result, error) {
	if err := f.storageCall(a, ActionVolumeDelete, fmt.Sprintf("%s snapshot=%v", volume, finalSnapshot), actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.findKind(region, KindVolume, volume)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][v].State != "available" {
		return Result{}, errors.New("VolumeInUse: " + volume + " is attached to an instance")
	}
	out := Result{ResourceID: volume}
	if finalSnapshot {
		out.Snapshots = map[string]string{volume: f.add(region, Resource{Kind: KindSnapshot, State: "pending",
			Links: []Ref{{Kind: KindVolume, ID: volume}}})}
		v, _ = f.findKind(region, KindVolume, volume)
	}
	f.drop(region, v)
	out.RequestID = f.request()
	return out, nil
}

func (f *Fake) CreateSnapshot(_ context.Context, a Account, actor Actor, region, volume, name string) (Result, error) {
	if err := f.storageCall(a, ActionSnapshotCreate, volume, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.findKind(region, KindVolume, volume); err != nil {
		return Result{}, err
	}
	id := f.add(region, Resource{Kind: KindSnapshot, Name: name, State: "pending", Links: []Ref{{Kind: KindVolume, ID: volume}}})
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) DeleteSnapshot(_ context.Context, a Account, actor Actor, region, snapshot string) (Result, error) {
	if err := f.storageCall(a, ActionSnapshotDelete, snapshot, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteSnapshot(region, snapshot)
}

func (f *Fake) deleteSnapshot(region, snapshot string) (Result, error) {
	s, err := f.findKind(region, KindSnapshot, snapshot)
	if err != nil {
		return Result{}, err
	}
	for _, r := range f.Resources[region] {
		if r.Kind != KindImage {
			continue
		}
		for _, l := range r.Links {
			if l.Kind == KindSnapshot && l.ID == snapshot {
				return Result{}, errors.New("InvalidSnapshot.InUse: " + snapshot + " is in use by " + r.ID)
			}
		}
	}
	f.drop(region, s)
	return Result{ResourceID: snapshot, RequestID: f.request()}, nil
}

func (f *Fake) CreateImage(_ context.Context, a Account, actor Actor, region, instance, name string, reboot bool) (Result, error) {
	if err := f.storageCall(a, ActionImageCreate, fmt.Sprintf("%s reboot=%v", instance, reboot), actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, instance)
	if err != nil {
		return Result{}, err
	}
	var links []Ref
	for _, l := range f.Resources[region][i].Links {
		if l.Kind == KindVolume {
			links = append(links, Ref{Kind: KindSnapshot, ID: f.add(region, Resource{Kind: KindSnapshot, State: "pending",
				Links: []Ref{{Kind: KindVolume, ID: l.ID}}}), Name: l.Name})
			i, _ = f.find(region, instance)
		}
	}
	id := f.add(region, Resource{Kind: KindImage, Name: name, State: "pending", Links: links,
		Tags: map[string]string{"argus:from-instance": instance}})
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) DeleteImage(_ context.Context, a Account, actor Actor, region, image string, withSnapshots bool) (Result, error) {
	if err := f.storageCall(a, ActionImageDelete, fmt.Sprintf("%s snapshots=%v", image, withSnapshots), actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindImage, image)
	if err != nil {
		return Result{}, err
	}
	links := f.Resources[region][i].Links
	f.drop(region, i)
	out := Result{ResourceID: image, RequestID: f.request()}
	if withSnapshots {
		n := 0
		for _, l := range links {
			if l.Kind == KindSnapshot {
				if _, err := f.deleteSnapshot(region, l.ID); err == nil {
					n++
				}
			}
		}
		out.Detail = fmt.Sprintf("Deleted %d of its snapshots", n)
	}
	return out, nil
}

func (f *Fake) AllocateAddress(_ context.Context, a Account, actor Actor, region, name string) (Result, error) {
	if err := f.storageCall(a, ActionAddressAllocate, region, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ip := fmt.Sprintf("203.0.113.%d", f.made+10)
	id := f.add(region, Resource{Kind: KindAddress, Name: name, PublicIP: ip, State: "unattached"})
	return Result{ResourceID: id, RequestID: f.request(), Detail: "Address " + ip}, nil
}

func (f *Fake) AssociateAddress(_ context.Context, a Account, actor Actor, region, address, instance string) (Result, error) {
	if err := f.storageCall(a, ActionAddressAssociate, address+" "+instance, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.findKind(region, KindAddress, address)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][e].State == "attached" {
		return Result{}, errors.New("Resource.AlreadyAssociated: " + address + " is in use")
	}
	i, err := f.find(region, instance)
	if err != nil {
		return Result{}, err
	}
	f.Resources[region][e].State = "attached"
	f.link(region, e, Ref{Kind: KindInstance, ID: instance})
	f.Resources[region][i].PublicIP = f.Resources[region][e].PublicIP
	return Result{ResourceID: address, RequestID: f.request()}, nil
}

func (f *Fake) DisassociateAddress(_ context.Context, a Account, actor Actor, region, address string) (Result, error) {
	if err := f.storageCall(a, ActionAddressDisassociate, address, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.findKind(region, KindAddress, address)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][e].State != "attached" {
		return Result{}, errors.New("the address is not attached to anything")
	}
	f.Resources[region][e].State = "unattached"
	f.Resources[region][e].Links = nil
	return Result{ResourceID: address, RequestID: f.request()}, nil
}

func (f *Fake) ReleaseAddress(_ context.Context, a Account, actor Actor, region, address string) (Result, error) {
	if err := f.storageCall(a, ActionAddressRelease, address, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.findKind(region, KindAddress, address)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][e].State == "attached" {
		return Result{}, errors.New("InvalidIPAddress.InUse: " + address + " is attached")
	}
	f.drop(region, e)
	return Result{ResourceID: address, RequestID: f.request()}, nil
}

// ManageMissing answers Missing.
func (f *Fake) ManageMissing(_ context.Context, a Account, _ string) ([]string, error) {
	f.record("manage-missing")
	if err := f.admit(a); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.Missing...), nil
}

// Needs names each storage action's permission as AWS would.
func (f *Fake) Needs(a Action) []string {
	if IsStorage(a) {
		return []string{"ec2:" + string(a)}
	}
	return nil
}

var (
	_ Storage       = (*Fake)(nil)
	_ PolicyChecker = (*Fake)(nil)
)
