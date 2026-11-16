package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Capacity Reservations: room kept for instances of one type in one zone.
Creating one bills its room from the moment AWS has it, whether anything
runs in it or not; cancelling stops that. Both are Change level and go
through the usual plan, approval and apply path. A launch can name one to
land in it (planLaunch).
*/

type cloudCapacityCreateSpec struct {
	Zone     string     `json:"zone"`
	Size     string     `json:"size"`
	Count    int32      `json:"count"`
	Targeted bool       `json:"targeted"`
	EndAt    *time.Time `json:"endAt,omitempty"`
	Name     string     `json:"name,omitempty"`
}

type cloudCapacityCancelSpec struct {
	Size  string `json:"size"`
	Zone  string `json:"zone"`
	Total int    `json:"total"`
	Used  int    `json:"used"`
}

// maxCapacityCount is the most room one reservation Argus makes keeps.
const maxCapacityCount = 100

var reZone = regexp.MustCompile(`^[a-z0-9-]+[a-z]$`)

func isCapacity(a cloud.Action) bool {
	return a == cloud.ActionCapacityCreate || a == cloud.ActionCapacityCancel
}

func (a *API) planCapacity(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.Capacitor); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot reserve capacity from Argus yet")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ch.Environment = m.acct.Environment
	if in.Action == cloud.ActionCapacityCreate {
		var s cloudCapacityCreateSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("a capacity reservation takes {zone, size, count}")
		}
		s.Name = strings.TrimSpace(s.Name)
		if !strings.HasPrefix(s.Zone, in.Region) || !reZone.MatchString(s.Zone) {
			return ch, http.StatusBadRequest, errors.New("pick a zone in " + in.Region)
		}
		if !cloud.ValidSize(s.Size) {
			return ch, http.StatusBadRequest, errors.New("pick an instance type, like m6i.large")
		}
		if s.Count < 1 || s.Count > maxCapacityCount {
			return ch, http.StatusBadRequest, fmt.Errorf("keep room for 1 to %d instances", maxCapacityCount)
		}
		if len(s.Name) > 128 {
			return ch, http.StatusBadRequest, errors.New("keep the name under 128 characters")
		}
		if s.EndAt != nil {
			end := s.EndAt.UTC()
			if end.Before(time.Now().Add(time.Hour)) || end.After(time.Now().AddDate(5, 0, 0)) {
				return ch, http.StatusBadRequest, errors.New("end it at least an hour from now, and within five years")
			}
			s.EndAt = &end
		}
		ch.Spec, _ = json.Marshal(s)
		ch.ResourceName = s.Name
		ch.Summary = fmt.Sprintf("Reserve room for %d %s in %s", s.Count, s.Size, s.Zone)
		if s.Targeted {
			ch.Summary += " for launches that name it"
		}
		if s.EndAt != nil {
			ch.Summary += ", until " + s.EndAt.Format("2 Jan 2006 15:04 UTC")
		} else {
			ch.Summary += ", until cancelled"
		}
		if p, ok := cloud.HourlyPrice(s.Size); ok {
			ch.Summary += fmt.Sprintf(" (about $%.2f a month while empty, at list price)", float64(s.Count)*p*cloud.HoursPerMonth)
		}
		return ch, http.StatusOK, nil
	}
	cr, err := a.cloudResource(r.Context(), m.acct.ID, in.Region, cloud.KindCapacityReservation, in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, errors.New("Argus has not read that capacity reservation in " + in.Region)
	}
	if cr.State != "active" && cr.State != "pending" && cr.State != "scheduled" {
		return ch, http.StatusConflict, errors.New(cr.Name + " is " + cr.State + "; only an active one can be cancelled")
	}
	total, _ := strconv.Atoi(cr.Details["total"])
	free, _ := strconv.Atoi(cr.Details["available"])
	s := cloudCapacityCancelSpec{Size: cr.Size, Zone: cr.Zone, Total: total, Used: total - free}
	ch.Spec, _ = json.Marshal(s)
	ch.ResourceID, ch.ResourceName = cr.ID, cr.Name
	ch.Environment = cloudEnvOf(cr.Tags, m.acct.Environment)
	ch.Summary = fmt.Sprintf("Cancel %s (room for %d %s in %s)", cr.Name, total, cr.Size, cr.Zone)
	if s.Used > 0 {
		ch.Summary += fmt.Sprintf("; the %d instance%s in it keep running at the usual rate", s.Used, plural(s.Used))
	}
	return ch, http.StatusOK, nil
}

func (a *API) applyCapacity(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	cp, ok := m.manager.(cloud.Capacitor)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot reserve capacity from Argus yet")
	}
	actor := cloud.Actor(ch.RequestedBy)
	if ch.Action == cloud.ActionCapacityCancel {
		return cp.CancelCapacity(ctx, m.target, actor, ch.Region, ch.ResourceID)
	}
	var s cloudCapacityCreateSpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	res, err := cp.CreateCapacity(ctx, m.target, actor, cloud.CapacitySpec{Region: ch.Region, Zone: s.Zone, Size: s.Size, Count: s.Count,
		Targeted: s.Targeted, EndAt: s.EndAt, Name: s.Name,
		Tags: map[string]string{"argus:change": ch.ID, "argus:launched-by": ch.RequestedBy}})
	if err == nil && res.ResourceID != "" {
		res.Detail = "Made " + res.ResourceID + "; AWS bills its room from now"
	}
	return res, err
}

// launchCapacity checks the capacity reservation a launch names: read in
// this account and region, active, in the subnet's zone, for the size, and
// with room for every instance of the launch.
func (a *API) launchCapacity(ctx context.Context, m cloudManaged, region, id, zone, size string, count int) (cloud.Resource, error) {
	cr, err := a.cloudResource(ctx, m.acct.ID, region, cloud.KindCapacityReservation, id)
	if err != nil {
		return cr, errors.New("Argus has not read capacity reservation " + id + " in " + region)
	}
	if cr.State != "active" {
		return cr, errors.New(cr.Name + " is " + cr.State + ", not active")
	}
	if cr.Zone != zone {
		return cr, fmt.Errorf("%s keeps room in %s; pick a subnet there", cr.Name, cr.Zone)
	}
	if cr.Size != size {
		return cr, fmt.Errorf("%s keeps room for %s, not %s", cr.Name, cr.Size, size)
	}
	if free, _ := strconv.Atoi(cr.Details["available"]); free < count {
		return cr, fmt.Errorf("%s has room for %d more, not %d", cr.Name, free, count)
	}
	return cr, nil
}
