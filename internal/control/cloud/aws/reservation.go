package aws

import (
	"context"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// reservations reads Reserved Instances and Capacity Reservations in each
// region, and Savings Plans once for the account. Describe calls are free.
// Each part that fails is left out of the snapshot's kinds, so its last
// read stands, and the marker says which part could not be read.
func reservations(ctx context.Context, cfg awssdk.Config, regions []string) (got []cloud.Resource, kinds []cloud.Kind, fails []string) {
	mark := cloud.Resource{Kind: cloud.KindReservationsRead, ID: "reservations", Name: "Reservations", Region: cloud.GlobalRegion,
		Details: map[string]string{}}
	var ris, crs []cloud.Resource
	riOK, crOK := true, true
	for _, region := range regions {
		c := cfg.Copy()
		c.Region = region
		client := ec2.NewFromConfig(c)
		if rs, err := reservedInstances(ctx, client, region); err != nil {
			riOK = false
			fails = append(fails, "Reserved Instances in "+region+": "+plain(err))
		} else {
			ris = append(ris, rs...)
		}
		if rs, err := capacityReservations(ctx, client, region); err != nil {
			crOK = false
			fails = append(fails, "Capacity Reservations in "+region+": "+plain(err))
		} else {
			crs = append(crs, rs...)
		}
	}
	if riOK {
		kinds = append(kinds, cloud.KindReservedInstance)
		got = append(got, ris...)
	}
	if crOK {
		kinds = append(kinds, cloud.KindCapacityReservation)
		got = append(got, crs...)
	}
	// Savings Plans have one endpoint, in us-east-1.
	c := cfg.Copy()
	c.Region = "us-east-1"
	if rs, err := savingsPlans(ctx, savingsplans.NewFromConfig(c)); err != nil {
		mark.Details["savingsPlans"] = "no"
		if denied(err) {
			mark.Details["savingsPlansWhy"] = "update"
		}
		fails = append(fails, "Savings Plans: "+plain(err))
	} else {
		mark.Details["savingsPlans"] = "yes"
		kinds = append(kinds, cloud.KindSavingsPlan)
		got = append(got, rs...)
	}
	mark.Details["reservedInstances"] = yesNo(riOK)
	mark.Details["capacityReservations"] = yesNo(crOK)
	kinds = append(kinds, cloud.KindReservationsRead)
	got = append(got, mark)
	return got, kinds, fails
}

// denied is an error from a role without the permission.
func denied(err error) bool {
	s := err.Error()
	return strings.Contains(s, "AccessDenied") || strings.Contains(s, "UnauthorizedOperation") || strings.Contains(s, "not authorized")
}

func reservedInstances(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	out, err := c.DescribeReservedInstances(ctx, &ec2.DescribeReservedInstancesInput{
		Filters: []types.Filter{{Name: awssdk.String("state"), Values: []string{"active", "payment-pending"}}},
	})
	if err != nil {
		return nil, err
	}
	var rs []cloud.Resource
	for _, ri := range out.ReservedInstances {
		rs = append(rs, reservedInstance(ri, region))
	}
	return rs, nil
}

// reservedInstance is one Reserved Instance as a resource. hourly is the
// usage price plus any hourly recurring charge, per instance; upfront is
// the fixed price paid once, per instance.
func reservedInstance(ri types.ReservedInstances, region string) cloud.Resource {
	id := awssdk.ToString(ri.ReservedInstancesId)
	count := awssdk.ToInt32(ri.InstanceCount)
	size := string(ri.InstanceType)
	r := cloud.Resource{Kind: cloud.KindReservedInstance, ID: id, Name: strconv.Itoa(int(count)) + " × " + size, Region: region,
		Size: size, State: string(ri.State), CreatedAt: ri.Start, Tags: tagMap(ri.Tags), Details: map[string]string{}}
	r.Details["count"] = strconv.Itoa(int(count))
	r.Details["scope"] = "region"
	if ri.Scope == types.ScopeAvailabilityZone {
		r.Details["scope"] = "zone"
		r.Zone = awssdk.ToString(ri.AvailabilityZone)
	}
	hourly := float64(awssdk.ToFloat32(ri.UsagePrice))
	for _, rc := range ri.RecurringCharges {
		if rc.Frequency == types.RecurringChargeFrequencyHourly {
			hourly += awssdk.ToFloat64(rc.Amount)
		}
	}
	set(r.Details, "hourly", money(hourly))
	set(r.Details, "upfront", money(float64(awssdk.ToFloat32(ri.FixedPrice))))
	if d := awssdk.ToInt64(ri.Duration); d > 0 {
		// AWS sells one- and three-year terms.
		r.Details["termMonths"] = "12"
		if d > 2*365*86400 {
			r.Details["termMonths"] = "36"
		}
	}
	set(r.Details, "payment", string(ri.OfferingType))
	set(r.Details, "class", string(ri.OfferingClass))
	set(r.Details, "platform", string(ri.ProductDescription))
	set(r.Details, "tenancy", string(ri.InstanceTenancy))
	set(r.Details, "start", timeString(ri.Start))
	set(r.Details, "end", timeString(ri.End))
	if name := r.Tags["Name"]; name != "" {
		r.Name = name + " (" + r.Name + ")"
	}
	return r
}

func capacityReservations(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var rs []cloud.Resource
	pg := ec2.NewDescribeCapacityReservationsPaginator(c, &ec2.DescribeCapacityReservationsInput{
		Filters: []types.Filter{{Name: awssdk.String("state"), Values: []string{"active", "pending", "scheduled", "payment-pending", "assessing", "delayed"}}},
	})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cr := range page.CapacityReservations {
			rs = append(rs, capacityReservation(cr, region))
		}
	}
	return rs, nil
}

func capacityReservation(cr types.CapacityReservation, region string) cloud.Resource {
	id := awssdk.ToString(cr.CapacityReservationId)
	tags := tagMap(cr.Tags)
	r := cloud.Resource{Kind: cloud.KindCapacityReservation, ID: id, Name: cloud.Name(tags, id), Region: region,
		Zone: awssdk.ToString(cr.AvailabilityZone), Size: awssdk.ToString(cr.InstanceType), State: string(cr.State),
		CreatedAt: cr.CreateDate, Tags: tags, Details: map[string]string{}}
	r.Details["total"] = strconv.Itoa(int(awssdk.ToInt32(cr.TotalInstanceCount)))
	r.Details["available"] = strconv.Itoa(int(awssdk.ToInt32(cr.AvailableInstanceCount)))
	set(r.Details, "platform", string(cr.InstancePlatform))
	set(r.Details, "match", string(cr.InstanceMatchCriteria))
	set(r.Details, "tenancy", string(cr.Tenancy))
	start := cr.StartDate
	if start == nil {
		start = cr.CreateDate
	}
	set(r.Details, "start", timeString(start))
	if cr.EndDateType == types.EndDateTypeLimited {
		set(r.Details, "end", timeString(cr.EndDate))
	}
	return r
}

func savingsPlans(ctx context.Context, c *savingsplans.Client) ([]cloud.Resource, error) {
	var rs []cloud.Resource
	in := &savingsplans.DescribeSavingsPlansInput{States: []sptypes.SavingsPlanState{
		sptypes.SavingsPlanStateActive, sptypes.SavingsPlanStatePaymentPending, sptypes.SavingsPlanStateQueued}}
	for {
		out, err := c.DescribeSavingsPlans(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, sp := range out.SavingsPlans {
			rs = append(rs, savingsPlan(sp))
		}
		if awssdk.ToString(out.NextToken) == "" {
			return rs, nil
		}
		in.NextToken = out.NextToken
	}
}

func savingsPlan(sp sptypes.SavingsPlan) cloud.Resource {
	id := awssdk.ToString(sp.SavingsPlanId)
	kind := string(sp.SavingsPlanType)
	name := map[string]string{"Compute": "Compute Savings Plan", "EC2Instance": "EC2 Instance Savings Plan",
		"SageMaker": "SageMaker Savings Plan", "Database": "Database Savings Plan"}[kind]
	if name == "" {
		name = kind + " Savings Plan"
	}
	region := awssdk.ToString(sp.Region)
	if fam := awssdk.ToString(sp.Ec2InstanceFamily); fam != "" {
		name += " (" + fam + ", " + region + ")"
	}
	r := cloud.Resource{Kind: cloud.KindSavingsPlan, ID: id, Name: name, Region: cloud.GlobalRegion, State: string(sp.State),
		Tags: sp.Tags, Details: map[string]string{}}
	if t := parseTime(awssdk.ToString(sp.Start)); t != nil {
		r.CreatedAt = t
		r.Details["start"] = t.UTC().Format(time.RFC3339)
	}
	if t := parseTime(awssdk.ToString(sp.End)); t != nil {
		r.Details["end"] = t.UTC().Format(time.RFC3339)
	}
	r.Details["type"] = kind
	set(r.Details, "commitment", awssdk.ToString(sp.Commitment))
	set(r.Details, "family", awssdk.ToString(sp.Ec2InstanceFamily))
	set(r.Details, "planRegion", region)
	set(r.Details, "payment", string(sp.PaymentOption))
	set(r.Details, "upfront", awssdk.ToString(sp.UpfrontPaymentAmount))
	set(r.Details, "recurring", awssdk.ToString(sp.RecurringPaymentAmount))
	if sp.TermDurationInSeconds > 0 {
		r.Details["termMonths"] = "12"
		if sp.TermDurationInSeconds > 2*365*86400 {
			r.Details["termMonths"] = "36"
		}
	}
	return r
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func timeString(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func money(v float64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
