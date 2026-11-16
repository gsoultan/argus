package control

import (
	"math"
	"net/http"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud › Reservations: Reserved Instances, Savings Plans and Capacity
Reservations in every account, what each covers and when it ends. Read
from the stored inventory; use is the estimate cloud.AnalyseReservations
works out against the instances running at the last read.
*/

// cloudReservationsAccount says, for one account, which reservation kinds
// the last read could see, so an empty list is not mistaken for "none".
type cloudReservationsAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	// Read is false before the first reservations read.
	Read                 bool `json:"read"`
	ReservedInstances    bool `json:"reservedInstances"`
	CapacityReservations bool `json:"capacityReservations"`
	SavingsPlans         bool `json:"savingsPlans"`
	// SavingsPlansNeedRole is a read role made before Savings Plans were
	// read, which AWS refused.
	SavingsPlansNeedRole bool `json:"savingsPlansNeedRole"`
}

type cloudRIItem struct {
	CloudItem
	Use cloud.RIUse `json:"use"`
}

type cloudSPItem struct {
	CloudItem
	Use cloud.SPUse `json:"use"`
}

type cloudCoverageItem struct {
	cloud.Coverage
	AccountName string `json:"accountName"`
}

func (a *API) registerCloudReservationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/reservations", a.getCloudReservations)
}

func (a *API) getCloudReservations(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	accounts, err := a.store.CloudAccounts(ctx)
	if err != nil {
		a.fail(w, "cloud reservations", err)
		return
	}
	items := map[cloud.Kind][]CloudItem{}
	var all []cloud.AccountResource
	for _, k := range []cloud.Kind{cloud.KindInstance, cloud.KindReservedInstance, cloud.KindSavingsPlan, cloud.KindCapacityReservation, cloud.KindReservationsRead} {
		got, err := a.store.CloudResources(ctx, k)
		if err != nil {
			a.fail(w, "cloud reservations", err)
			return
		}
		items[k] = got
		for _, it := range got {
			all = append(all, cloud.AccountResource{Account: it.AccountID, Resource: it.Resource})
		}
	}
	rep := cloud.AnalyseReservations(all)
	// What Risks & waste calls paid for and not used, so both say the same.
	now := time.Now().UTC()
	byAccount := map[string][]cloud.Resource{}
	for _, x := range all {
		byAccount[x.Account] = append(byAccount[x.Account], x.Resource)
	}
	unused := map[string]float64{}
	var unusedMonthly float64
	for acct, rs := range byAccount {
		for _, f := range cloud.Findings(rs, nil, now) {
			if f.Check == "reservation-unused" || f.Check == "capacity-unused" || f.Check == "savings-plan-idle" {
				unused[acct+"/"+f.ResourceID] = f.Monthly
				unusedMonthly += f.Monthly
			}
		}
	}
	riUse := map[string]cloud.RIUse{}
	for _, u := range rep.RIs {
		riUse[u.Account+"/"+u.ID] = u
	}
	spUse := map[string]cloud.SPUse{}
	for _, u := range rep.Plans {
		spUse[u.Account+"/"+u.ID] = u
	}
	names := map[string]string{}
	seen := []cloudReservationsAccount{}
	read := map[string]cloud.Resource{}
	for _, it := range items[cloud.KindReservationsRead] {
		read[it.AccountID] = it.Resource
	}
	for _, c := range accounts {
		names[c.ID] = c.Name
		acc := cloudReservationsAccount{ID: c.ID, Name: c.Name, Environment: c.Environment}
		if m, ok := read[c.ID]; ok {
			acc.Read = true
			acc.ReservedInstances = m.Details["reservedInstances"] == "yes"
			acc.CapacityReservations = m.Details["capacityReservations"] == "yes"
			acc.SavingsPlans = m.Details["savingsPlans"] == "yes"
			acc.SavingsPlansNeedRole = m.Details["savingsPlansWhy"] == "update"
		}
		seen = append(seen, acc)
	}
	ris := []cloudRIItem{}
	for _, it := range items[cloud.KindReservedInstance] {
		ris = append(ris, cloudRIItem{CloudItem: it, Use: riUse[it.AccountID+"/"+it.Resource.ID]})
	}
	sps := []cloudSPItem{}
	for _, it := range items[cloud.KindSavingsPlan] {
		sps = append(sps, cloudSPItem{CloudItem: it, Use: spUse[it.AccountID+"/"+it.Resource.ID]})
	}
	crs := items[cloud.KindCapacityReservation]
	if crs == nil {
		crs = []CloudItem{}
	}
	cov := []cloudCoverageItem{}
	for _, c := range rep.Coverage {
		cov = append(cov, cloudCoverageItem{Coverage: c, AccountName: names[c.Account]})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": seen, "reservedInstances": ris, "savingsPlans": sps, "capacityReservations": crs,
		"coverage": cov, "unused": unused, "unusedMonthly": math.Round(unusedMonthly*100) / 100, "checkedAt": now,
	})
}
