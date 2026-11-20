package azure

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Costs, from Azure Cost Management, with the Reader assignment: Reader may
read a subscription's costs, so nothing more is asked of the setup command.
Azure does not charge for these calls.

A read is two calls: the spend by day and service from the first of last
month, and the forecast from today to the month's end.

Spend is read in US dollars, Azure's own conversion (the CostUSD figure),
because Argus's budgets and its totals across accounts are in dollars. A
subscription invoiced in another currency shows here what Azure says it
comes to in dollars, which is not to the cent what its invoice says.

The forecast is used only when Azure gives it in US dollars: the forecast
call answers in the billing currency, and Argus does not convert it.

Some kinds of subscription have no Cost Management (some free and
sponsored offers, and subscriptions bought through some partners). Azure
says so in its own words, which Argus passes on.
*/

// costAPI is the Microsoft.CostManagement version costs are read with.
const costAPI = "2023-11-01"

// ErrCostsLimited is Azure asking for fewer cost reads.
var ErrCostsLimited = errors.New("Azure is limiting how often costs are read for this subscription; Argus reads them again at its next read")

// costTable is how Cost Management answers a query: named columns, and
// rows of values in their order.
type costTable struct {
	Properties struct {
		NextLink string `json:"nextLink"`
		Columns  []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"properties"`
}

func (t costTable) column(name string) int {
	for i, c := range t.Properties.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// usageDate reads the day of a row: Azure gives 20261109 as a number, and
// in some answers a timestamp.
func usageDate(v any) (string, bool) {
	switch d := v.(type) {
	case float64:
		n := int(d)
		t, err := time.Parse("20060102", fmt.Sprintf("%08d", n))
		if err != nil {
			return "", false
		}
		return t.Format(time.DateOnly), true
	case string:
		if len(d) >= 10 {
			if t, err := time.Parse(time.DateOnly, d[:10]); err == nil {
				return t.Format(time.DateOnly), true
			}
		}
		if t, err := time.Parse("20060102", d); err == nil {
			return t.Format(time.DateOnly), true
		}
	}
	return "", false
}

// costQuery posts body to one of Cost Management's calls and follows its
// pages, giving each page to each.
func (p *Provider) costQuery(ctx context.Context, t token, u string, body any, each func(costTable) error) error {
	for range maxPages {
		var out costTable
		if _, err := p.send(ctx, t, cloud.SyncActor, http.MethodPost, u, body, &out); err != nil {
			return err
		}
		if err := each(out); err != nil {
			return err
		}
		next := out.Properties.NextLink
		if next == "" {
			return nil
		}
		if !strings.HasPrefix(next, p.ep.arm+"/") {
			return errors.New("Azure's answer pointed at another address for its next page")
		}
		u = next
	}
	return errors.New("Azure's cost answer did not end")
}

func costErr(err error) error {
	if msg := strings.ToLower(err.Error()); strings.Contains(msg, "too many requests") || strings.Contains(msg, "toomanyrequests") {
		return ErrCostsLimited
	}
	return err
}

// Costs reads this month's and last month's spend, and the forecast.
func (p *Provider) Costs(ctx context.Context, a cloud.Account, now time.Time) (cloud.Costs, error) {
	t, _, err := p.session(ctx, a, cloud.SyncActor)
	if err != nil {
		return cloud.Costs{}, err
	}
	now = now.UTC()
	this := cloud.MonthStart(now)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	next := this.AddDate(0, 1, 0)
	base := p.ep.arm + SubscriptionScope(a.AccountID) + "/providers/Microsoft.CostManagement/"
	stamp := func(d time.Time) string { return d.Format("2006-01-02T15:04:05Z") }

	query := map[string]any{
		"type": "ActualCost", "timeframe": "Custom",
		"timePeriod": map[string]string{"from": stamp(this.AddDate(0, -1, 0)), "to": stamp(today.AddDate(0, 0, 1).Add(-time.Second))},
		"dataset": map[string]any{
			"granularity": "Daily",
			"aggregation": map[string]any{"totalCost": map[string]string{"name": "CostUSD", "function": "Sum"}},
			"grouping":    []map[string]string{{"type": "Dimension", "name": "ServiceName"}},
		},
	}
	var rows []cloud.CostRow
	err = p.costQuery(ctx, t, base+"query?api-version="+costAPI, query, func(tb costTable) error {
		cost, date, svc := tb.column("CostUSD"), tb.column("UsageDate"), tb.column("ServiceName")
		if len(tb.Properties.Rows) > 0 && (cost < 0 || date < 0) {
			return errors.New("Azure's cost answer lacks the cost or the date")
		}
		for _, r := range tb.Properties.Rows {
			if cost >= len(r) || date >= len(r) {
				continue
			}
			amt, ok := r[cost].(float64)
			day, dok := usageDate(r[date])
			if !ok || !dok {
				continue
			}
			name := ""
			if svc >= 0 && svc < len(r) {
				name, _ = r[svc].(string)
			}
			if name == "" {
				name = "Other"
			}
			rows = append(rows, cloud.CostRow{Date: day, Service: name, Amount: amt})
		}
		return nil
	})
	if err != nil {
		return cloud.Costs{}, costErr(err)
	}

	// The forecast is Azure's prediction from today on; what was spent
	// before today is added to it for the month's expected end. On the
	// last day there is nothing left to forecast. A forecast Azure will
	// not give, or gives in another currency, is left out, not an error.
	var forecast, rest *float64
	if today.AddDate(0, 0, 1).Before(next) {
		ask := map[string]any{
			"type": "ActualCost", "timeframe": "Custom",
			"timePeriod": map[string]string{"from": stamp(today), "to": stamp(next.Add(-time.Second))},
			"dataset": map[string]any{
				"granularity": "Daily",
				"aggregation": map[string]any{"totalCost": map[string]string{"name": "Cost", "function": "Sum"}},
			},
			"includeActualCost": false, "includeFreshPartialCost": false,
		}
		sum, dollars, some := 0.0, true, false
		ferr := p.costQuery(ctx, t, base+"forecast?api-version="+costAPI, ask, func(tb costTable) error {
			cost, cur, status := tb.column("Cost"), tb.column("Currency"), tb.column("CostStatus")
			for _, r := range tb.Properties.Rows {
				if cost < 0 || cost >= len(r) {
					continue
				}
				if status >= 0 && status < len(r) {
					if s, _ := r[status].(string); !strings.EqualFold(s, "Forecast") {
						continue
					}
				}
				amt, ok := r[cost].(float64)
				if !ok {
					continue
				}
				c := ""
				if cur >= 0 && cur < len(r) {
					c, _ = r[cur].(string)
				}
				dollars = dollars && strings.EqualFold(c, "USD")
				sum += amt
				some = true
			}
			return nil
		})
		if ferr == nil && some && dollars {
			before := 0.0
			for _, r := range rows {
				if d, err := time.Parse(time.DateOnly, r.Date); err == nil && !d.Before(this) && d.Before(today) {
					before += r.Amount
				}
			}
			f, r := before+sum, math.Round(sum*100)/100
			forecast, rest = &f, &r
		}
	}
	costs := cloud.SumCosts(now, "USD", rows, forecast)
	costs.ForecastRest = rest
	return costs, nil
}

var _ cloud.CostReader = (*Provider)(nil)
