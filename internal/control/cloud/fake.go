package cloud

import (
	"context"
	"errors"
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
