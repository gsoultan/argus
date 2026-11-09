package control

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 6: costs, idle resources and budgets.

Costs come from the provider's billing API with the read role. AWS charges
a cent a request there, so each account is read every CloudCostInterval
(two requests: spend by day and service, and the forecast), and a person
may ask for a fresh read at most once every cloudCostAskEvery. That is
about $1.80 a month an account.

Idle resources are worked out from the inventory, which costs nothing: see
cloud.Idle. Each one names the change that ends its bill, when there is
one, and the console offers it through the usual change path.

Budgets are monthly, for one account or all of them. After every read,
each budget's spend is set against its thresholds (80% and 100% unless
changed) and, when asked, against the forecast; each alerts the people who
read the whole organization once a month.
*/

// CloudCostInterval is how often each account's costs are read.
const CloudCostInterval = 8 * time.Hour

// cloudCostAskEvery is how soon after a read a person may ask for another.
const cloudCostAskEvery = 15 * time.Minute

func (a *API) registerCloudCostRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/costs", a.getCloudCosts)
	mux.HandleFunc("POST /api/v1/cloud/costs/{id}/read", a.postCloudCostsRead)
	mux.HandleFunc("GET /api/v1/cloud/idle", a.getCloudIdle)
	mux.HandleFunc("POST /api/v1/cloud/budgets", a.saveCloudBudget(false))
	mux.HandleFunc("PUT /api/v1/cloud/budgets/{id}", a.saveCloudBudget(true))
	mux.HandleFunc("DELETE /api/v1/cloud/budgets/{id}", a.deleteCloudBudget)
}

// RunCloudCosts reads every account whose costs are due, in every
// organization, and checks budgets, each interval until ctx ends.
func (a *API) RunCloudCosts(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.readDueCloudCosts(tctx, time.Now().UTC())
		})
		if err != nil {
			a.log.Error("cloud costs", "error", err)
		}
		t.Reset(interval)
	}
}

// readDueCloudCosts reads the accounts last read, or last failed, more
// than CloudCostInterval ago, then checks the budgets.
func (a *API) readDueCloudCosts(ctx context.Context, now time.Time) {
	reads, err := a.store.CloudCostReads(ctx)
	if err != nil {
		a.log.Error("cloud costs: cannot list accounts", "error", err)
		return
	}
	due := func(t *time.Time) bool { return t == nil || now.Sub(*t) >= CloudCostInterval }
	read := false
	for _, r := range reads {
		if ctx.Err() != nil {
			return
		}
		if !due(r.ReadAt) || (r.ErrorAt != nil && !due(r.ErrorAt)) {
			continue
		}
		c, err := a.store.CloudAccount(ctx, r.AccountID)
		if err != nil {
			continue
		}
		a.readCloudCosts(ctx, c, now)
		read = true
	}
	if read {
		a.checkCloudBudgets(ctx, now)
	}
}

// readCloudCosts reads one account's costs and stores the answer, or why
// there was none.
func (a *API) readCloudCosts(ctx context.Context, c CloudAccount, now time.Time) error {
	p, ok := a.cloudProvider(c.Provider)
	if !ok {
		return errors.New("this control plane has no " + c.Provider + " provider")
	}
	cr, ok := p.(cloud.CostReader)
	if !ok {
		return errors.New("this provider does not read costs")
	}
	ext, err := a.cloudExternalID(ctx, c.Provider, c.AccountID)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	costs, err := cr.Costs(rctx, cloud.Account{Provider: c.Provider, AccountID: c.AccountID, RoleARN: c.RoleARN, ExternalID: ext}, now)
	if err != nil {
		if serr := a.store.SetCloudCostsError(ctx, c.ID, err.Error(), now); serr != nil {
			a.log.Error("cloud costs: cannot record a failed read", "account", c.AccountID, "error", serr)
		}
		return err
	}
	return a.store.SaveCloudCosts(ctx, c.ID, costs, now)
}

// cloudBudgetView is a budget with where it stands this month.
type cloudBudgetView struct {
	CloudBudget
	AccountName string `json:"accountName,omitempty"`
	// Spent is this month's spend in the budget's accounts so far.
	Spent float64 `json:"spent"`
	// Forecast is the month's expected end, nil when an account has none.
	Forecast *float64 `json:"forecast"`
	// Unread counts the budget's accounts with no read for this month.
	Unread int `json:"unread"`
}

// standing works out each budget's month so far from the reads.
func standing(budgets []CloudBudget, reads []CloudCostRead, now time.Time) []cloudBudgetView {
	month := cloud.MonthStart(now).Format("2006-01")
	out := make([]cloudBudgetView, 0, len(budgets))
	for _, b := range budgets {
		v := cloudBudgetView{CloudBudget: b}
		forecast, whole := 0.0, true
		for _, r := range reads {
			if b.AccountID != "" && r.AccountID != b.AccountID {
				continue
			}
			if b.AccountID != "" {
				v.AccountName = r.AccountName
			}
			if r.Costs == nil || r.Costs.Month != month {
				v.Unread++
				whole = false
				continue
			}
			v.Spent += r.Costs.MonthToDate
			if r.Costs.Forecast == nil {
				whole = false
			} else {
				forecast += *r.Costs.Forecast
			}
		}
		v.Spent = math.Round(v.Spent*100) / 100
		if whole {
			f := math.Round(forecast*100) / 100
			v.Forecast = &f
		}
		out = append(out, v)
	}
	return out
}

// checkCloudBudgets alerts on each threshold a budget's spend has passed
// this month, and on a forecast over the amount, once a month each.
func (a *API) checkCloudBudgets(ctx context.Context, now time.Time) {
	budgets, err := a.store.CloudBudgets(ctx)
	if err != nil || len(budgets) == 0 {
		return
	}
	reads, err := a.store.CloudCostReads(ctx)
	if err != nil {
		return
	}
	month := cloud.MonthStart(now).Format("2006-01")
	for _, v := range standing(budgets, reads, now) {
		pct := v.Spent / v.Amount * 100
		passed := 0
		for _, t := range v.Thresholds {
			if pct >= float64(t) && t > passed {
				passed = t
			}
		}
		if passed > 0 {
			// Every threshold passed is marked, so a jump from 50% to 110%
			// alerts once, for 100%.
			fresh := false
			for _, t := range v.Thresholds {
				if t <= passed {
					ok, err := a.store.MarkCloudBudgetAlerted(ctx, v.ID, month, t)
					if err != nil {
						a.log.Error("cloud budgets: cannot record an alert", "budget", v.ID, "error", err)
						return
					}
					fresh = fresh || (ok && t == passed)
				}
			}
			if fresh {
				sev := "warning"
				if passed >= 100 {
					sev = "critical"
				}
				a.raiseAlert(ctx, ConsoleAlert{Kind: AlertServer, Severity: sev, Audience: AudienceAdmins,
					Title:  fmt.Sprintf("Budget %q is at %d%%", v.Name, int(pct)),
					Detail: fmt.Sprintf("%s of %s spent in %s, %s.", money(v.Spent), money(v.Amount), budgetScope(v), monthWords(now)),
					Link:   "/cloud?tab=costs", Subject: "cloud-budget:" + v.ID})
			}
			continue
		}
		if v.Forecast != nil && v.forecastOver() {
			ok, err := a.store.MarkCloudBudgetAlerted(ctx, v.ID, month, 0)
			if err == nil && ok {
				a.raiseAlert(ctx, ConsoleAlert{Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
					Title: fmt.Sprintf("Budget %q is forecast to run over", v.Name),
					Detail: fmt.Sprintf("AWS expects %s by the end of %s against %s; %s spent so far in %s.",
						money(*v.Forecast), monthWords(now), money(v.Amount), money(v.Spent), budgetScope(v)),
					Link: "/cloud?tab=costs", Subject: "cloud-budget:" + v.ID})
			}
		}
	}
}

// forecastOver reports whether the forecast, and not yet the spend,
// runs over the amount and the budget asks to hear of it.
func (v cloudBudgetView) forecastOver() bool {
	return v.CloudBudget.Forecast && v.Forecast != nil && *v.Forecast > v.Amount && v.Spent < v.Amount
}

func budgetScope(v cloudBudgetView) string {
	if v.AccountID == "" {
		return "every account"
	}
	if v.AccountName != "" {
		return v.AccountName
	}
	return "its account"
}

func monthWords(t time.Time) string { return t.UTC().Format("January") }

// money is US dollars as a person writes them: $1,234.50.
func money(f float64) string {
	s := strconv.FormatFloat(math.Abs(f), 'f', 2, 64)
	whole, cents := s[:len(s)-3], s[len(s)-3:]
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	sign := ""
	if f < 0 {
		sign = "-"
	}
	return sign + "$" + b.String() + cents
}

func (a *API) getCloudCosts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	reads, err := a.store.CloudCostReads(r.Context())
	if err != nil {
		a.fail(w, "cloud costs", err)
		return
	}
	budgets, err := a.store.CloudBudgets(r.Context())
	if err != nil {
		a.fail(w, "cloud budgets", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": reads, "budgets": standing(budgets, reads, time.Now().UTC()),
		"intervalHours": int(CloudCostInterval.Hours()), "askEveryMinutes": int(cloudCostAskEvery.Minutes()),
	})
}

func (a *API) postCloudCostsRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "reading costs")
	if !ok {
		return
	}
	c, err := a.store.CloudAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	ok, err = a.store.AskCloudCosts(r.Context(), c.ID, cloudCostAskEvery)
	if err != nil {
		a.fail(w, "cloud costs", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("costs were read in the last %d minutes; AWS charges for each read, so try again later",
			int(cloudCostAskEvery.Minutes())))
		return
	}
	now := time.Now().UTC()
	rerr := a.readCloudCosts(r.Context(), c, now)
	a.auditAsset(r, AuditEvent{Action: "cloud.costs.read", Severity: "info", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name), Detail: "read costs now"})
	if rerr == nil {
		a.checkCloudBudgets(r.Context(), now)
	}
	reads, err := a.store.CloudCostReads(r.Context())
	if err != nil {
		a.fail(w, "cloud costs", err)
		return
	}
	for _, x := range reads {
		if x.AccountID == c.ID {
			writeJSON(w, http.StatusOK, x)
			return
		}
	}
	writeErr(w, http.StatusNotFound, ErrCloudAccountNotFound.Error())
}

// cloudIdleItem is an idle resource with the account it is in.
type cloudIdleItem struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Environment string `json:"environment"`
	cloud.IdleFinding
}

// cloudIdleKinds are the kinds idle resources are found among.
var cloudIdleKinds = []cloud.Kind{cloud.KindVolume, cloud.KindAddress, cloud.KindInstance, cloud.KindSnapshot,
	cloud.KindImage, cloud.KindLoadBalancer, cloud.KindTargetGroup}

func (a *API) getCloudIdle(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	type acct struct{ name, env string }
	byAccount := map[string][]cloud.Resource{}
	accounts := map[string]acct{}
	var order []string
	for _, k := range cloudIdleKinds {
		items, err := a.store.CloudResources(r.Context(), k)
		if err != nil {
			a.fail(w, "cloud resources", err)
			return
		}
		for _, it := range items {
			if _, ok := accounts[it.AccountID]; !ok {
				accounts[it.AccountID] = acct{it.AccountName, it.Environment}
				order = append(order, it.AccountID)
			}
			byAccount[it.AccountID] = append(byAccount[it.AccountID], it.Resource)
		}
	}
	now := time.Now().UTC()
	out := []cloudIdleItem{}
	total := 0.0
	for _, id := range order {
		for _, f := range cloud.Idle(byAccount[id], now) {
			out = append(out, cloudIdleItem{AccountID: id, AccountName: accounts[id].name, Environment: accounts[id].env, IdleFinding: f})
			total += f.Monthly
		}
	}
	slices.SortStableFunc(out, func(x, y cloudIdleItem) int {
		switch {
		case x.Monthly > y.Monthly:
			return -1
		case x.Monthly < y.Monthly:
			return 1
		}
		return 0
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "monthly": math.Round(total*100) / 100})
}

type cloudBudgetIn struct {
	AccountID  string  `json:"accountId"`
	Name       string  `json:"name"`
	Amount     float64 `json:"amount"`
	Thresholds []int   `json:"thresholds"`
	Forecast   bool    `json:"forecast"`
}

func (in *cloudBudgetIn) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 80 {
		return errors.New("name the budget, in at most 80 characters")
	}
	if math.IsNaN(in.Amount) || in.Amount <= 0 || in.Amount > 10_000_000 {
		return errors.New("the amount is a monthly figure in US dollars, more than 0")
	}
	in.Amount = math.Round(in.Amount*100) / 100
	if len(in.Thresholds) == 0 {
		in.Thresholds = []int{80, 100}
	}
	slices.Sort(in.Thresholds)
	in.Thresholds = slices.Compact(in.Thresholds)
	if len(in.Thresholds) > 5 {
		return errors.New("at most five alert thresholds")
	}
	for _, t := range in.Thresholds {
		if t < 1 || t > 200 {
			return errors.New("alert thresholds are percentages of the amount, 1 to 200")
		}
	}
	return nil
}

func (a *API) saveCloudBudget(update bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.cloudAdmin(w, r, "changing budgets")
		if !ok {
			return
		}
		var in cloudBudgetIn
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := in.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		target := "every account"
		if in.AccountID != "" {
			c, err := a.store.CloudAccount(r.Context(), in.AccountID)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "pick a connected account, or every account")
				return
			}
			in.AccountID, target = c.ID, cloudTarget(c.Provider, c.AccountID, c.Name)
		}
		b := CloudBudget{AccountID: in.AccountID, Name: in.Name, Amount: in.Amount, Thresholds: in.Thresholds,
			Forecast: in.Forecast, CreatedBy: actor}
		what := "created"
		if update {
			b.ID, what = r.PathValue("id"), "changed"
		}
		saved, err := a.store.SaveCloudBudget(r.Context(), b)
		if errors.Is(err, ErrCloudBudgetNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud budget", err)
			return
		}
		a.auditAsset(r, AuditEvent{Action: "cloud.budget." + what, Severity: "info", ActorEmail: actor, Target: target,
			Detail: fmt.Sprintf("budget %q: %s a month, alerts at %v%%", saved.Name, money(saved.Amount), saved.Thresholds)})
		// A new or changed budget may already be over.
		a.checkCloudBudgets(r.Context(), time.Now().UTC())
		code := http.StatusCreated
		if update {
			code = http.StatusOK
		}
		writeJSON(w, code, saved)
	}
}

func (a *API) deleteCloudBudget(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "changing budgets")
	if !ok {
		return
	}
	b, err := a.store.DeleteCloudBudget(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudBudgetNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud budget", err)
		return
	}
	a.closeAlerts(r.Context(), "cloud-budget:"+b.ID, "Budget removed by "+actor)
	a.auditAsset(r, AuditEvent{Action: "cloud.budget.removed", Severity: "info", ActorEmail: actor, Target: "budget " + b.Name,
		Detail: fmt.Sprintf("budget %q of %s a month", b.Name, money(b.Amount))})
	w.WriteHeader(http.StatusNoContent)
}
