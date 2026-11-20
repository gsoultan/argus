package azure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const costColumns = `"columns":[{"name":"CostUSD","type":"Number"},{"name":"UsageDate","type":"Number"},{"name":"ServiceName","type":"String"},{"name":"Currency","type":"String"}]`

func TestCostsAreReadByDayAndServiceInDollars(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", true)
	m.mu.Lock()
	m.costs = map[string]string{
		"query": `{"properties":{"nextLink":"NEXT/query?api-version=2023-11-01&page=2",` + costColumns + `,"rows":[
			[100.0,20261001,"Virtual Machines","USD"],[20.5,20261015,"Storage","USD"],[40.0,20261030,"Virtual Machines","USD"],
			[30.0,20261101,"Virtual Machines","USD"],[5.25,20261102,"Storage","USD"]]}}`,
		"query?page=2": `{"properties":{` + costColumns + `,"rows":[[12.0,20261110,"Virtual Machines","USD"],[1.0,20261110,"","USD"],["bad",20261110,"Storage","USD"]]}}`,
		"forecast": `{"properties":{"columns":[{"name":"Cost"},{"name":"UsageDate"},{"name":"CostStatus"},{"name":"Currency"}],"rows":[
			[10.0,20261110,"Forecast","USD"],[10.5,20261111,"Forecast","USD"],[99.0,20261109,"Actual","USD"]]}}`,
	}
	m.mu.Unlock()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	c, err := p.Costs(context.Background(), acct, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Month != "2026-11" || c.Currency != "USD" || c.MonthToDate != 48.25 || c.LastMonth != 160.5 || c.LastMonthToDate != 100 {
		t.Fatalf("totals: %+v", c)
	}
	// Spent before today (35.25) and what Azure predicts from today on.
	if c.Forecast == nil || *c.Forecast != 55.75 || c.ForecastRest == nil || *c.ForecastRest != 20.5 {
		t.Fatalf("forecast: %v %v", c.Forecast, c.ForecastRest)
	}
	if len(c.Services) != 3 || c.Services[0].Service != "Virtual Machines" || c.Services[0].ThisMonth != 42 || c.Services[0].LastMonth != 140 ||
		c.Services[2].Service != "Other" {
		t.Fatalf("services: %+v", c.Services)
	}
	if len(c.Days) != 41 || c.Days[0].Date != "2026-10-01" || c.Days[0].Amount != 100 || c.Days[40].Amount != 13 {
		t.Fatalf("days: %d %+v", len(c.Days), c.Days[len(c.Days)-1])
	}
	m.mu.Lock()
	asked := append([]string(nil), m.costAsked...)
	m.mu.Unlock()
	if len(asked) != 3 || !strings.Contains(asked[0], `"from":"2026-10-01T00:00:00Z","to":"2026-11-10T23:59:59Z"`) || !strings.Contains(asked[0], `"name":"CostUSD"`) ||
		!strings.Contains(asked[0], `"ServiceName"`) || !strings.HasPrefix(asked[1], "query ") ||
		!strings.Contains(asked[2], `"from":"2026-11-10T00:00:00Z","to":"2026-11-30T23:59:59Z"`) || !strings.Contains(asked[2], `"includeActualCost":false`) {
		t.Fatalf("what was asked: %q", asked)
	}
}

func TestAForecastInAnotherCurrencyOrOnTheLastDayIsLeftOut(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", true)
	m.mu.Lock()
	m.costs = map[string]string{
		"query": `{"properties":{` + costColumns + `,"rows":[[30.0,20261101,"Virtual Machines","USD"]]}}`,
		"forecast": `{"properties":{"columns":[{"name":"Cost"},{"name":"UsageDate"},{"name":"CostStatus"},{"name":"Currency"}],"rows":[
			[160000.0,20261110,"Forecast","IDR"]]}}`,
	}
	m.mu.Unlock()
	c, err := p.Costs(context.Background(), acct, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	if err != nil || c.Forecast != nil || c.ForecastRest != nil || c.MonthToDate != 30 {
		t.Fatalf("a forecast in rupiah must not be added to dollars: %+v %v", c, err)
	}
	m.mu.Lock()
	m.costAsked = nil
	m.mu.Unlock()
	c, err = p.Costs(context.Background(), acct, time.Date(2026, 11, 30, 9, 0, 0, 0, time.UTC))
	m.mu.Lock()
	n := len(m.costAsked)
	m.mu.Unlock()
	if err != nil || c.Forecast != nil || n != 1 {
		t.Fatalf("the last day has nothing to forecast: %+v %v, %d calls", c, err, n)
	}
}

func TestWhyCostsCouldNotBeReadIsSaid(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", true)
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	m.mu.Lock()
	m.costStatus = 429
	m.mu.Unlock()
	if _, err := p.Costs(context.Background(), acct, now); !errors.Is(err, ErrCostsLimited) {
		t.Fatalf("limited: %v", err)
	}
	m.mu.Lock()
	m.costStatus = 400
	m.mu.Unlock()
	if _, err := p.Costs(context.Background(), acct, now); err == nil || !strings.Contains(err.Error(), "not supported for this offer type") {
		t.Fatalf("an offer without Cost Management: %v", err)
	}
	// A next page elsewhere is not followed.
	m.mu.Lock()
	m.costStatus = 0
	m.costs = map[string]string{"query": `{"properties":{"nextLink":"https://elsewhere.example/steal",` + costColumns + `,"rows":[]}}`}
	m.mu.Unlock()
	if _, err := p.Costs(context.Background(), acct, now); err == nil || !strings.Contains(err.Error(), "another address") {
		t.Fatalf("a next page elsewhere: %v", err)
	}
	var _ cloud.CostReader = p
}
