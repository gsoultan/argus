package aws

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Quoting and buying Reserved Instances and Savings Plans. A quote is a
Describe*Offerings call (free); a buy names the offering the quote found and,
for a Reserved Instance, the most AWS may charge upfront, so a price that
moved between the check and the call fails instead of charging more.
Reserved Instances bought here are regional, Linux/UNIX, default tenancy
and standard class, the kind that bends across sizes of a family.
*/

// Quote asks AWS for the offering that matches q, priced for the whole
// quantity.
func (p *Provider) Quote(ctx context.Context, a cloud.Account, actor cloud.Actor, q cloud.OfferQuery) (cloud.Offer, error) {
	switch q.Kind {
	case "ri":
		return p.quoteRI(ctx, a, actor, q)
	case "sp":
		return p.quoteSP(ctx, a, actor, q)
	}
	return cloud.Offer{}, cloud.ErrNoOffer
}

func (p *Provider) quoteRI(ctx context.Context, a cloud.Account, actor cloud.Actor, q cloud.OfferQuery) (cloud.Offer, error) {
	c, err := p.client(ctx, a, actor, q.Region)
	if err != nil {
		return cloud.Offer{}, err
	}
	secs := int64(q.TermMonths) / 12 * 31536000
	out, err := c.DescribeReservedInstancesOfferings(ctx, &ec2.DescribeReservedInstancesOfferingsInput{
		InstanceType: types.InstanceType(q.Size), ProductDescription: types.RIProductDescription("Linux/UNIX"),
		OfferingClass: types.OfferingClassTypeStandard, OfferingType: types.OfferingTypeValues(q.Payment),
		InstanceTenancy: types.TenancyDefault, IncludeMarketplace: awssdk.Bool(false),
		MinDuration: awssdk.Int64(secs), MaxDuration: awssdk.Int64(secs),
		Filters: []types.Filter{{Name: awssdk.String("scope"), Values: []string{"Region"}}},
	})
	if err != nil {
		return cloud.Offer{}, errors.New(plain(err))
	}
	for _, o := range out.ReservedInstancesOfferings {
		if o.Scope != types.ScopeRegional || (o.CurrencyCode != "" && o.CurrencyCode != types.CurrencyCodeValuesUsd) {
			continue
		}
		hourly := float64(awssdk.ToFloat32(o.UsagePrice))
		for _, rc := range o.RecurringCharges {
			if rc.Frequency == types.RecurringChargeFrequencyHourly {
				hourly += awssdk.ToFloat64(rc.Amount)
			}
		}
		n := float64(q.Count)
		return cloud.Offer{ID: awssdk.ToString(o.ReservedInstancesOfferingId),
			Upfront: float64(awssdk.ToFloat32(o.FixedPrice)) * n, Hourly: hourly * n}.Price(q.TermMonths), nil
	}
	return cloud.Offer{}, cloud.ErrNoOffer
}

func (p *Provider) spClient(ctx context.Context, a cloud.Account, actor cloud.Actor) (*savingsplans.Client, error) {
	// Savings Plans have one endpoint, in us-east-1.
	cfg, err := p.assumed(ctx, a, actor, "us-east-1")
	if err != nil {
		return nil, err
	}
	return savingsplans.NewFromConfig(cfg), nil
}

func (p *Provider) quoteSP(ctx context.Context, a cloud.Account, actor cloud.Actor, q cloud.OfferQuery) (cloud.Offer, error) {
	c, err := p.spClient(ctx, a, actor)
	if err != nil {
		return cloud.Offer{}, err
	}
	secs := int64(q.TermMonths) / 12 * 31536000
	in := &savingsplans.DescribeSavingsPlansOfferingsInput{
		PlanTypes: []sptypes.SavingsPlanType{sptypes.SavingsPlanType(q.PlanType)}, Durations: []int64{secs},
		PaymentOptions: []sptypes.SavingsPlanPaymentOption{sptypes.SavingsPlanPaymentOption(q.Payment)},
		Currencies:     []sptypes.CurrencyCode{sptypes.CurrencyCodeUsd},
	}
	if q.PlanType == cloud.PlanEC2Instance {
		in.Filters = []sptypes.SavingsPlanOfferingFilterElement{
			{Name: sptypes.SavingsPlanOfferingFilterAttributeRegion, Values: []string{q.Region}},
			{Name: sptypes.SavingsPlanOfferingFilterAttributeInstanceFamily, Values: []string{q.Family}},
		}
	}
	out, err := c.DescribeSavingsPlansOfferings(ctx, in)
	if err != nil {
		return cloud.Offer{}, errors.New(plain(err))
	}
	for _, o := range out.SearchResults {
		if o.DurationSeconds != secs || string(o.PaymentOption) != q.Payment {
			continue
		}
		up := cloud.SPUpfront(q.Commitment, q.TermMonths, q.Payment)
		return cloud.Offer{ID: awssdk.ToString(o.OfferingId), Upfront: up,
			Hourly: q.Commitment - up/cloud.TermHours(q.TermMonths)}.Price(q.TermMonths), nil
	}
	return cloud.Offer{}, cloud.ErrNoOffer
}

// Buy buys the offering o names. A Reserved Instance's upfront is capped at
// what was quoted.
func (p *Provider) Buy(ctx context.Context, a cloud.Account, actor cloud.Actor, q cloud.OfferQuery, o cloud.Offer) (cloud.Result, error) {
	if q.Kind == "ri" {
		c, err := p.client(ctx, a, actor, q.Region)
		if err != nil {
			return cloud.Result{}, err
		}
		in := &ec2.PurchaseReservedInstancesOfferingInput{InstanceCount: awssdk.Int32(q.Count),
			ReservedInstancesOfferingId: awssdk.String(o.ID)}
		if o.Upfront > 0 {
			in.LimitPrice = &types.ReservedInstanceLimitPrice{Amount: awssdk.Float64(o.Upfront), CurrencyCode: types.CurrencyCodeValuesUsd}
		}
		out, err := c.PurchaseReservedInstancesOffering(ctx, in)
		if err != nil {
			return cloud.Result{}, errors.New(plain(err))
		}
		return cloud.Result{ResourceID: awssdk.ToString(out.ReservedInstancesId), RequestID: requestID(out.ResultMetadata)}, nil
	}
	c, err := p.spClient(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	in := &savingsplans.CreateSavingsPlanInput{SavingsPlanOfferingId: awssdk.String(o.ID),
		Commitment: awssdk.String(strconv.FormatFloat(q.Commitment, 'f', 3, 64))}
	if q.Token != "" {
		in.ClientToken = awssdk.String(q.Token)
	}
	if q.Payment == cloud.PayPartial {
		in.UpfrontPaymentAmount = awssdk.String(fmt.Sprintf("%.2f", o.Upfront))
	}
	out, err := c.CreateSavingsPlan(ctx, in)
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: awssdk.ToString(out.SavingsPlanId), RequestID: requestID(out.ResultMetadata)}, nil
}
