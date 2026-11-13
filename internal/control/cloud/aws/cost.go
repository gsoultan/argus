package aws

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Costs, from Cost Explorer, with the read role. Cost Explorer charges $0.01
a request, so a read is two requests (the spend by day and service, and the
forecast) and the control plane makes one every few hours, not at every
sync. Spend is filtered to the account itself, so connecting an
organization's management account shows that account's own bill, not the
whole organization's.
*/

// costRegion is where Cost Explorer answers, for every account.
const costRegion = "us-east-1"

type costAPI interface {
	GetCostAndUsage(context.Context, *costexplorer.GetCostAndUsageInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
	GetCostForecast(context.Context, *costexplorer.GetCostForecastInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostForecastOutput, error)
}

// newCostClient makes the Cost Explorer client; tests replace it.
var newCostClient = func(cfg awssdk.Config) costAPI { return costexplorer.NewFromConfig(cfg) }

// Costs reads this month's and last month's spend, and the forecast.
func (p *Provider) Costs(ctx context.Context, a cloud.Account, now time.Time) (cloud.Costs, error) {
	cfg, err := p.assumed(ctx, a, cloud.SyncActor, costRegion)
	if err != nil {
		return cloud.Costs{}, err
	}
	return readCosts(ctx, newCostClient(cfg), a.AccountID, now)
}

func readCosts(ctx context.Context, c costAPI, accountID string, now time.Time) (cloud.Costs, error) {
	now = now.UTC()
	this := cloud.MonthStart(now)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	own := &cetypes.Expression{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionLinkedAccount, Values: []string{accountID}}}
	in := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{Start: awssdk.String(this.AddDate(0, -1, 0).Format(time.DateOnly)),
			End: awssdk.String(today.AddDate(0, 0, 1).Format(time.DateOnly))},
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		GroupBy:     []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: awssdk.String("SERVICE")}},
		Filter:      own,
	}
	var rows []cloud.CostRow
	currency := "USD"
	for pages := 0; pages < 20; pages++ {
		out, err := c.GetCostAndUsage(ctx, in)
		if err != nil {
			return cloud.Costs{}, costErr(err)
		}
		for _, r := range out.ResultsByTime {
			date := ""
			if r.TimePeriod != nil {
				date = awssdk.ToString(r.TimePeriod.Start)
			}
			for _, g := range r.Groups {
				m, ok := g.Metrics["UnblendedCost"]
				if !ok || len(g.Keys) == 0 {
					continue
				}
				amt, _ := strconv.ParseFloat(awssdk.ToString(m.Amount), 64)
				if u := awssdk.ToString(m.Unit); u != "" {
					currency = u
				}
				rows = append(rows, cloud.CostRow{Date: date, Service: g.Keys[0], Amount: amt})
			}
		}
		if out.NextPageToken == nil {
			break
		}
		in.NextPageToken = out.NextPageToken
	}

	// The forecast is asked from today to the month's end, but with
	// MONTHLY granularity AWS answers for the whole month: its Total
	// already holds what was spent before today, and is the figure the
	// Billing home page shows as the month's forecast. Adding the spend
	// before today to it counts that spend twice. ForecastRest is the part
	// that is AWS's prediction. On the last day there is nothing to
	// forecast and AWS refuses; with too little history it refuses too.
	var forecast, rest *float64
	if next := this.AddDate(0, 1, 0); today.Before(next) && !today.AddDate(0, 0, 1).Equal(next) {
		out, err := c.GetCostForecast(ctx, &costexplorer.GetCostForecastInput{
			TimePeriod:  &cetypes.DateInterval{Start: awssdk.String(today.Format(time.DateOnly)), End: awssdk.String(next.Format(time.DateOnly))},
			Granularity: cetypes.GranularityMonthly, Metric: cetypes.MetricUnblendedCost, Filter: own,
		})
		if err == nil && out.Total != nil {
			if f, err := strconv.ParseFloat(awssdk.ToString(out.Total.Amount), 64); err == nil {
				before := 0.0
				for _, r := range rows {
					if d, err := time.Parse(time.DateOnly, r.Date); err == nil && !d.Before(this) && d.Before(today) {
						before += r.Amount
					}
				}
				r := max(f-before, 0)
				forecast, rest = &f, &r
			}
		}
	}
	costs := cloud.SumCosts(now, currency, rows, forecast)
	if rest != nil {
		r := math.Round(*rest*100) / 100
		costs.ForecastRest = &r
	}
	return costs, nil
}

func costErr(err error) error {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case "AccessDeniedException", "AccessDenied", "UnauthorizedOperation":
			return cloud.ErrCostsDenied
		case "DataUnavailableException":
			return cloud.ErrCostsUnavailable
		}
	}
	return errors.New(plain(err))
}

var _ cloud.CostReader = (*Provider)(nil)
