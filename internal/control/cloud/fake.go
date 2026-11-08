package cloud

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory provider for tests. It records every call with the
// actor it ran for, so a test can assert the source identity as well as the
// result.
type Fake struct {
	mu sync.Mutex
	// Name is what Kind returns; "aws" when empty.
	Name string
	// BaseIdentity and BaseErr are what Base answers.
	BaseIdentity Base
	BaseErr      error
	// Roles maps a role ARN to the external id it requires. Verify and
	// Inventory refuse any other pair, as a trust policy would.
	Roles map[string]string
	// Regions is what Verify reports.
	Regions []string
	// Resources is the inventory, by region.
	Resources map[string][]Resource
	// RegionErr fails Inventory for one region.
	RegionErr map[string]error
	// Calls is every Verify and Inventory, as "verify <actor>" or
	// "inventory <region> <actor>".
	Calls []string
	// Now is the read time; time.Now when nil.
	Now func() time.Time
	// Protected is termination protection, by instance id.
	Protected map[string]bool
	// LastUserData is what the last launch would have run at first boot.
	LastUserData string

	reqs, launched int
}

// ErrAccessDenied is the fake's answer to a role it would not let in.
var ErrAccessDenied = errors.New("AccessDenied: not authorized to perform sts:AssumeRole")

func (f *Fake) Kind() string {
	if f.Name == "" {
		return "aws"
	}
	return f.Name
}

func (f *Fake) Base(context.Context) (Base, error) {
	if f.BaseErr != nil {
		return Base{}, f.BaseErr
	}
	return f.BaseIdentity, nil
}

func (f *Fake) admit(a Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	want, ok := f.Roles[a.RoleARN]
	if !ok || want != a.ExternalID {
		return ErrAccessDenied
	}
	return nil
}

func (f *Fake) Verify(_ context.Context, a Account, actor Actor) (Identity, error) {
	f.record("verify " + string(actor))
	if err := f.admit(a); err != nil {
		return Identity{}, err
	}
	return Identity{ARN: a.RoleARN, AccountID: a.AccountID, Regions: append([]string(nil), f.Regions...)}, nil
}

func (f *Fake) Inventory(_ context.Context, a Account, region string, kinds []Kind) (Snapshot, error) {
	f.record("inventory " + region + " " + string(SyncActor))
	if err := f.admit(a); err != nil {
		return Snapshot{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.RegionErr[region]; err != nil {
		return Snapshot{}, err
	}
	want := map[Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []Resource
	for _, r := range f.Resources[region] {
		if want[r.Kind] {
			out = append(out, r)
		}
	}
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	return Snapshot{Region: region, Kinds: kinds, Resources: out, ReadAt: now}, nil
}

func (f *Fake) record(s string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, s)
	f.mu.Unlock()
}

// CallLog returns a copy of the calls so far.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

// The fake as a Manager: changes edit Resources in place, so the next
// Inventory sees them, and every call is recorded with its actor.

func (f *Fake) find(region, id string) (int, error) {
	for i, r := range f.Resources[region] {
		if r.ID == id && r.Kind == KindInstance {
			return i, nil
		}
	}
	return -1, errors.New("InvalidInstanceID.NotFound: no instance " + id + " in " + region)
}

func (f *Fake) request() string {
	f.reqs++
	return fmt.Sprintf("req-%04d", f.reqs)
}

func (f *Fake) Images(_ context.Context, a Account, actor Actor, region string) ([]Image, error) {
	f.record("images " + region + " " + string(actor))
	if err := f.admit(a); err != nil {
		return nil, err
	}
	return []Image{{Family: "ubuntu-24.04", Label: "Ubuntu 24.04 LTS", ID: "ami-ubuntu", Name: "ubuntu-noble", Linux: true}}, nil
}

func (f *Fake) Launch(_ context.Context, a Account, actor Actor, s LaunchSpec) (Result, error) {
	f.record("launch " + s.Name + " " + string(actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	f.launched++
	id := fmt.Sprintf("i-fake%04d", f.launched)
	tags := map[string]string{"Name": s.Name}
	for k, v := range s.Tags {
		tags[k] = v
	}
	f.Resources[s.Region] = append(f.Resources[s.Region], Resource{Kind: KindInstance, ID: id, Name: s.Name,
		Region: s.Region, State: "pending", Size: s.Size, Tags: tags, Details: map[string]string{}})
	if s.Protect {
		if f.Protected == nil {
			f.Protected = map[string]bool{}
		}
		f.Protected[id] = true
	}
	f.LastUserData = s.UserData
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) Power(_ context.Context, a Account, actor Actor, region, id string, act Action) (Result, error) {
	f.record(string(act) + " " + id + " " + string(actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, id)
	if err != nil {
		return Result{}, err
	}
	switch act {
	case ActionStart:
		f.Resources[region][i].State = "running"
	case ActionStop:
		f.Resources[region][i].State = "stopped"
	}
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) Resize(_ context.Context, a Account, actor Actor, region, id, size string) (Result, error) {
	f.record("resize " + id + " " + size + " " + string(actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, id)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][i].State != "stopped" {
		return Result{}, errors.New("IncorrectInstanceState: the instance must be stopped to change its type")
	}
	f.Resources[region][i].Size = size
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) SetTags(_ context.Context, a Account, actor Actor, region, id string, set map[string]string, remove []string) (Result, error) {
	f.record("tags " + id + " " + string(actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, id)
	if err != nil {
		return Result{}, err
	}
	r := &f.Resources[region][i]
	if r.Tags == nil {
		r.Tags = map[string]string{}
	}
	for k, v := range set {
		r.Tags[k] = v
	}
	for _, k := range remove {
		delete(r.Tags, k)
	}
	r.Name = Name(r.Tags, r.ID)
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) Protect(_ context.Context, a Account, actor Actor, region, id string, on bool) (Result, error) {
	f.record(fmt.Sprintf("protect %s %v %s", id, on, actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.find(region, id); err != nil {
		return Result{}, err
	}
	if f.Protected == nil {
		f.Protected = map[string]bool{}
	}
	f.Protected[id] = on
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) PlanRemove(_ context.Context, a Account, actor Actor, region, id string) (RemovePlan, error) {
	f.record("plan-remove " + id + " " + string(actor))
	if err := f.admit(a); err != nil {
		return RemovePlan{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, id)
	if err != nil {
		return RemovePlan{}, err
	}
	r := f.Resources[region][i]
	p := RemovePlan{ID: r.ID, Name: r.Name, State: r.State, Size: r.Size, Protected: f.Protected[id],
		PublicIP: r.PublicIP, Volumes: []Volume{}}
	if s := r.Tags["aws:cloudformation:stack-name"]; s != "" {
		p.ManagedBy = "CloudFormation stack " + s
	}
	for _, l := range r.Links {
		if l.Kind == KindVolume {
			p.Volumes = append(p.Volumes, Volume{ID: l.ID, Device: l.Name, SizeGB: 8, Deleted: true})
		}
	}
	return p, nil
}

func (f *Fake) Remove(_ context.Context, a Account, actor Actor, region, id string, finalSnapshots bool) (Result, error) {
	f.record(fmt.Sprintf("remove %s snapshots=%v %s", id, finalSnapshots, actor))
	if err := f.admit(a); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, id)
	if err != nil {
		return Result{}, err
	}
	if f.Protected[id] {
		return Result{}, ErrProtected
	}
	out := Result{ResourceID: id, RequestID: f.request()}
	if finalSnapshots {
		out.Snapshots = map[string]string{}
		for _, l := range f.Resources[region][i].Links {
			if l.Kind == KindVolume {
				out.Snapshots[l.ID] = "snap-" + l.ID
			}
		}
	}
	f.Resources[region][i].State = "shutting-down"
	return out, nil
}
