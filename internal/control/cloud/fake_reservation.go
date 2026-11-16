package cloud

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// CreateCapacity keeps the reservation with the account-wide resources,
// where the inventory reads it, as AWS's own would be.
func (f *Fake) CreateCapacity(_ context.Context, a Account, actor Actor, s CapacitySpec) (Result, error) {
	if err := f.storageCall(a, ActionCapacityCreate, s.Zone+" "+s.Size, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made++
	id := fmt.Sprintf("cr-fake%04d", f.made)
	match := "open"
	if s.Targeted {
		match = "targeted"
	}
	r := Resource{Kind: KindCapacityReservation, ID: id, Name: id, Region: s.Region, Zone: s.Zone, Size: s.Size, State: "active",
		Details: map[string]string{"total": strconv.Itoa(int(s.Count)), "available": strconv.Itoa(int(s.Count)), "match": match,
			"start": time.Now().UTC().Format(time.RFC3339)}}
	if s.Name != "" {
		r.Name = s.Name
	}
	if s.EndAt != nil {
		r.Details["end"] = s.EndAt.UTC().Format(time.RFC3339)
	}
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	f.Resources[GlobalRegion] = append(f.Resources[GlobalRegion], r)
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

// CancelCapacity takes the reservation out of what the inventory reads.
func (f *Fake) CancelCapacity(_ context.Context, a Account, actor Actor, region, id string) (Result, error) {
	if err := f.storageCall(a, ActionCapacityCancel, id, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.Resources[GlobalRegion] {
		if r.Kind == KindCapacityReservation && r.ID == id && r.Region == region {
			f.drop(GlobalRegion, i)
			return Result{ResourceID: id, RequestID: f.request()}, nil
		}
	}
	return Result{}, errors.New("InvalidCapacityReservationId.NotFound: " + id)
}
