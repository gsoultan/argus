package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"

	"github.com/gsoultan/argus/internal/control/cloud"
)

type fakeCE struct {
	pages    []*costexplorer.GetCostAndUsageOutput
	ins      []*costexplorer.GetCostAndUsageInput
	forecast *costexplorer.GetCostForecastInput
	err      error
}

func (f *fakeCE) GetCostAndUsage(_ context.Context, in *costexplorer.GetCostAndUsageInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := *in
	f.ins = append(f.ins, &cp)
	p := f.pages[0]
	f.pages = f.pages[1:]
	return p, nil
}

func (f *fakeCE) GetCostForecast(_ context.Context, in *costexplorer.GetCostForecastInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostForecastOutput, error) {
	f.forecast = in
	return &costexplorer.GetCostForecastOutput{Total: &cetypes.MetricValue{Amount: awssdk.String("100"), Unit: awssdk.String("USD")}}, nil
}

func day(date string, amounts map[string]string) cetypes.ResultByTime {
	r := cetypes.ResultByTime{TimePeriod: &cetypes.DateInterval{Start: awssdk.String(date)}}
	for svc, a := range amounts {
		r.Groups = append(r.Groups, cetypes.Group{Keys: []string{svc},
			Metrics: map[string]cetypes.MetricValue{"UnblendedCost": {Amount: awssdk.String(a), Unit: awssdk.String("USD")}}})
	}
	return r
}

func TestReadCosts(t *testing.T) {
	now := time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		{ResultsByTime: []cetypes.ResultByTime{day("2026-10-02", map[string]string{"EC2": "4"}), day("2026-11-01", map[string]string{"EC2": "3"})},
			NextPageToken: awssdk.String("more")},
		{ResultsByTime: []cetypes.ResultByTime{day("2026-11-08", map[string]string{"S3": "2"}), day("2026-11-09", map[string]string{"EC2": "1"})}},
	}}
	c, err := readCosts(context.Background(), f, "603914770215", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ins) != 2 || awssdk.ToString(f.ins[1].NextPageToken) != "more" {
		t.Fatalf("pages: %d", len(f.ins))
	}
	in := f.ins[0]
	if awssdk.ToString(in.TimePeriod.Start) != "2026-10-01" || awssdk.ToString(in.TimePeriod.End) != "2026-11-10" ||
		in.Filter.Dimensions.Values[0] != "603914770215" {
		t.Fatalf("asked for: %s..%s %+v", awssdk.ToString(in.TimePeriod.Start), awssdk.ToString(in.TimePeriod.End), in.Filter.Dimensions)
	}
	if awssdk.ToString(f.forecast.TimePeriod.Start) != "2026-11-09" || awssdk.ToString(f.forecast.TimePeriod.End) != "2026-12-01" {
		t.Fatalf("forecast for: %+v", f.forecast.TimePeriod)
	}
	// Spent before today (3 + 2) plus the forecast from today.
	if c.MonthToDate != 6 || c.LastMonth != 4 || c.Forecast == nil || *c.Forecast != 105 {
		t.Fatalf("costs: %+v", c)
	}
}

func TestReadCostsErrors(t *testing.T) {
	for code, want := range map[string]error{"AccessDeniedException": cloud.ErrCostsDenied, "DataUnavailableException": cloud.ErrCostsUnavailable} {
		f := &fakeCE{err: &smithy.GenericAPIError{Code: code, Message: "no"}}
		if _, err := readCosts(context.Background(), f, "1", time.Now()); !errors.Is(err, want) {
			t.Errorf("%s: %v", code, err)
		}
	}
}

func TestStoppedAt(t *testing.T) {
	if got := stoppedAt("User initiated (2026-10-01 12:30:00 GMT)"); got != "2026-10-01T12:30:00Z" {
		t.Fatalf("stopped at: %q", got)
	}
	if got := stoppedAt("User initiated"); got != "" {
		t.Fatalf("no time: %q", got)
	}
}

func TestReadPolicyReadsCosts(t *testing.T) {
	for _, a := range []string{"ce:GetCostAndUsage", "ce:GetCostForecast"} {
		found := false
		for _, p := range ReadPolicy {
			found = found || p == a
		}
		if !found {
			t.Errorf("the read policy lacks %s", a)
		}
	}
}
