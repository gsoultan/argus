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

// fakeDiscount is the share of the list price a commitment costs, by term.
func fakeDiscount(months int) float64 {
	if months == 36 {
		return 0.42
	}
	return 0.62
}

// Quote prices a commitment from the list price, as AWS would.
func (f *Fake) Quote(_ context.Context, a Account, actor Actor, q OfferQuery) (Offer, error) {
	f.record("quote " + q.Kind + " " + string(actor))
	if err := f.admit(a); err != nil {
		return Offer{}, err
	}
	f.mu.Lock()
	bump := f.PriceBump
	f.mu.Unlock()
	hours := TermHours(q.TermMonths)
	var o Offer
	switch q.Kind {
	case "ri":
		list, ok := HourlyPrice(q.Size)
		if !ok {
			return Offer{}, ErrNoOffer
		}
		all := (list*fakeDiscount(q.TermMonths) + bump) * float64(q.Count) * hours
		o = Offer{ID: fmt.Sprintf("ri-offer-%s-%d-%s", q.Size, q.TermMonths, q.Payment)}
		switch q.Payment {
		case PayAll:
			o.Upfront = round2(all)
		case PayPartial:
			o.Upfront = round2(all / 2)
			o.Hourly = all / 2 / hours
		default:
			o.Hourly = all / hours
		}
	case "sp":
		o = Offer{ID: fmt.Sprintf("sp-offer-%s-%d-%s", q.PlanType, q.TermMonths, q.Payment)}
		o.Upfront = SPUpfront(q.Commitment, q.TermMonths, q.Payment)
		o.Hourly = q.Commitment - o.Upfront/hours + bump
	default:
		return Offer{}, ErrNoOffer
	}
	return o.Price(q.TermMonths), nil
}

// Buy keeps the commitment with the account-wide resources, where the
// inventory reads it.
func (f *Fake) Buy(_ context.Context, a Account, actor Actor, q OfferQuery, o Offer) (Result, error) {
	act := ActionRIBuy
	if q.Kind == "sp" {
		act = ActionSPBuy
	}
	if err := f.storageCall(a, act, o.ID, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made++
	f.Bought = append(f.Bought, o.ID)
	now := time.Now().UTC()
	end := now.AddDate(0, q.TermMonths, 0)
	r := Resource{ID: fmt.Sprintf("bought-fake%04d", f.made), State: "active", Region: q.Region,
		Details: map[string]string{"termMonths": strconv.Itoa(q.TermMonths), "payment": q.Payment,
			"start": now.Format(time.RFC3339), "end": end.Format(time.RFC3339)}}
	if q.Kind == "ri" {
		r.Kind, r.Size, r.Name = KindReservedInstance, q.Size, fmt.Sprintf("%d × %s", q.Count, q.Size)
		r.Details["count"], r.Details["scope"], r.Details["platform"], r.Details["tenancy"] = strconv.Itoa(int(q.Count)), "region", "Linux/UNIX", "default"
		r.Details["hourly"] = strconv.FormatFloat(o.Hourly/float64(q.Count), 'f', 6, 64)
		r.Details["fixed"] = strconv.FormatFloat(o.Upfront/float64(q.Count), 'f', 2, 64)
	} else {
		r.Kind, r.Region, r.Name = KindSavingsPlan, GlobalRegion, q.PlanType+" Savings Plan"
		r.Details["type"], r.Details["commitment"] = q.PlanType, strconv.FormatFloat(q.Commitment, 'f', 3, 64)
		if q.PlanType == PlanEC2Instance {
			r.Details["family"], r.Details["planRegion"] = q.Family, q.Region
		}
	}
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	f.Resources[GlobalRegion] = append(f.Resources[GlobalRegion], r)
	return Result{ResourceID: r.ID, RequestID: f.request()}, nil
}
