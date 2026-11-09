package cloud

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

/*
Costs, idle resources and the list prices behind both.

Costs are what the provider bills, read from its billing API (AWS Cost
Explorer, which charges a cent a call, so the control plane reads it a few
times a day, not at every sync). Idle resources are worked out from the
inventory alone, so they cost nothing to find: a disk nothing uses, an
address on nothing, an instance stopped for weeks whose disks still bill,
an old snapshot of a disk that is gone, a load balancer with nothing
behind it.
*/

// Costs is one account's spending, as billed, for this month and last.
type Costs struct {
	// Month is the month MonthToDate is for: "2026-11".
	Month    string `json:"month"`
	Currency string `json:"currency"`
	// MonthToDate is this month's spend so far.
	MonthToDate float64 `json:"monthToDate"`
	// LastMonth is the whole of last month; LastMonthToDate is last month
	// up to the same day, which is the fair comparison mid-month.
	LastMonth       float64 `json:"lastMonth"`
	LastMonthToDate float64 `json:"lastMonthToDate"`
	// Forecast is what the provider expects this month to end at, nil
	// when it would not say (too little history, or the month's last day).
	Forecast *float64 `json:"forecast"`
	// Days is each day's total, from the first of last month to today.
	Days []DayCost `json:"days"`
	// Services is each service's spend, this month's biggest first.
	Services []ServiceCost `json:"services"`
}

// DayCost is one day's total. Date is "2026-11-09".
type DayCost struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
}

// ServiceCost is one service's spend this month and last.
type ServiceCost struct {
	Service   string  `json:"service"`
	ThisMonth float64 `json:"thisMonth"`
	LastMonth float64 `json:"lastMonth"`
}

// CostRow is one day's spend on one service, as a billing API answers.
type CostRow struct {
	Date    string
	Service string
	Amount  float64
}

// CostReader reads what an account has been billed.
type CostReader interface {
	// Costs reads from the first of last month to now, and the forecast to
	// the end of this month.
	Costs(ctx context.Context, a Account, now time.Time) (Costs, error)
}

var (
	// ErrCostsDenied is a read role made before costs were read, which
	// may not call the billing API.
	ErrCostsDenied = errors.New("the read role may not read costs yet. Update the account's argus-access stack from its row on the Cloud overview; Argus reads costs again at its next read")
	// ErrCostsUnavailable is an account where billing data is not on yet.
	ErrCostsUnavailable = errors.New("AWS has no cost data for this account yet. Open Cost Explorer once in the AWS console to turn it on; the first data takes up to a day")
)

// MonthStart is the first instant of t's month, in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// SumCosts folds a billing API's rows into Costs for now's month.
func SumCosts(now time.Time, currency string, rows []CostRow, forecast *float64) Costs {
	this := MonthStart(now)
	last := this.AddDate(0, -1, 0)
	// Last month to the same day: the 31st of a 30-day month is its end.
	sameDay := last.AddDate(0, 0, now.UTC().Day())
	if sameDay.After(this) {
		sameDay = this
	}
	c := Costs{Month: this.Format("2006-01"), Currency: currency, Forecast: forecast}
	days := map[string]float64{}
	svc := map[string]*ServiceCost{}
	for _, r := range rows {
		d, err := time.Parse(time.DateOnly, r.Date)
		if err != nil || d.Before(last) {
			continue
		}
		days[r.Date] += r.Amount
		s := svc[r.Service]
		if s == nil {
			s = &ServiceCost{Service: r.Service}
			svc[r.Service] = s
		}
		if d.Before(this) {
			c.LastMonth += r.Amount
			s.LastMonth += r.Amount
			if d.Before(sameDay) {
				c.LastMonthToDate += r.Amount
			}
		} else {
			c.MonthToDate += r.Amount
			s.ThisMonth += r.Amount
		}
	}
	end := now.UTC()
	for d := last; !d.After(end); d = d.AddDate(0, 0, 1) {
		k := d.Format(time.DateOnly)
		c.Days = append(c.Days, DayCost{Date: k, Amount: round2(days[k])})
	}
	for _, s := range svc {
		s.ThisMonth, s.LastMonth = round2(s.ThisMonth), round2(s.LastMonth)
		if s.ThisMonth == 0 && s.LastMonth == 0 {
			continue
		}
		c.Services = append(c.Services, *s)
	}
	slices.SortFunc(c.Services, func(a, b ServiceCost) int {
		if a.ThisMonth != b.ThisMonth {
			if a.ThisMonth > b.ThisMonth {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Service, b.Service)
	})
	c.MonthToDate, c.LastMonth, c.LastMonthToDate = round2(c.MonthToDate), round2(c.LastMonth), round2(c.LastMonthToDate)
	if c.Forecast != nil {
		// A forecast below what is already spent is not one.
		f := round2(max(*c.Forecast, c.MonthToDate))
		c.Forecast = &f
	}
	return c
}

func round2(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 2, 64), 64)
	return v
}

// HoursPerMonth is the hours list prices count in a month.
const HoursPerMonth = 730

// List prices, US dollars, us-east-1. Other regions differ by a few
// percent; everything priced with these says it is an estimate.
var (
	volumePerGB = map[string]float64{"gp3": 0.08, "gp2": 0.10, "io2": 0.125, "io1": 0.125, "st1": 0.045, "sc1": 0.015, "standard": 0.05}
	// SnapshotPerGB is a snapshot's monthly price per GB stored.
	SnapshotPerGB = 0.05
	// AddressMonthly is an Elastic IP's monthly price, attached or not.
	AddressMonthly = 0.005 * HoursPerMonth
	// LoadBalancerMonthly is an application or network load balancer's
	// monthly price before traffic.
	LoadBalancerMonthly = 0.0225 * HoursPerMonth
)

// VolumeMonthly is a disk's monthly list price from its Size ("100 GiB
// gp3"), and whether the size could be read.
func VolumeMonthly(size string) (float64, bool) {
	f := strings.Fields(size)
	if len(f) < 2 {
		return 0, false
	}
	gb, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, false
	}
	per, ok := volumePerGB[f[len(f)-1]]
	if !ok {
		per = volumePerGB["gp3"]
	}
	return round2(float64(gb) * per), true
}

// GiB is the number at the front of a Size ("100 GiB").
func GiB(size string) int {
	f := strings.Fields(size)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(f[0])
	return n
}

// Idle thresholds.
const (
	// IdleStoppedFor is how long an instance is stopped before its disks
	// count as idle.
	IdleStoppedFor = 14 * 24 * time.Hour
	// IdleSnapshotAge is how old a snapshot of a deleted disk, that no
	// image uses, is before it counts.
	IdleSnapshotAge = 90 * 24 * time.Hour
)

// IdleFinding is one resource that bills for nothing anyone uses.
type IdleFinding struct {
	Kind   Kind   `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Region string `json:"region"`
	// Why says what makes it idle, in words.
	Why string `json:"why"`
	// Monthly is the list price it bills a month; AtMost marks an upper
	// bound (snapshots are incremental, so most bill less).
	Monthly float64 `json:"monthly"`
	AtMost  bool    `json:"atMost,omitempty"`
	// Since is when it became idle, when known.
	Since *time.Time `json:"since,omitempty"`
	// Fix is the change that ends the bill, "" when the fix needs a
	// person's judgement (removing an instance, a load balancer).
	Fix Action `json:"fix,omitempty"`
}

// Idle finds what bills for nothing in one account's inventory, biggest
// bill first.
func Idle(rs []Resource, now time.Time) []IdleFinding {
	present := map[string]bool{}
	usedSnap := map[string]bool{}
	lbTargets := map[string]int{} // load balancer id → target groups with a target
	lbGroups := map[string]int{}
	vols := map[string]Resource{}
	for _, r := range rs {
		present[r.ID] = true
		switch r.Kind {
		case KindVolume:
			vols[r.ID] = r
		case KindImage:
			for _, l := range r.Links {
				if l.Kind == KindSnapshot {
					usedSnap[l.ID] = true
				}
			}
		case KindTargetGroup:
			for _, l := range r.Links {
				if l.Kind == KindLoadBalancer {
					lbGroups[l.ID]++
					if r.State == "healthy" || r.State == "degraded" {
						lbTargets[l.ID]++
					}
				}
			}
		}
	}
	var out []IdleFinding
	add := func(r Resource, why string, monthly float64, since *time.Time, fix Action) {
		out = append(out, IdleFinding{Kind: r.Kind, ID: r.ID, Name: r.Name, Region: r.Region, Why: why,
			Monthly: round2(monthly), Since: since, Fix: fix})
	}
	for _, r := range rs {
		switch r.Kind {
		case KindVolume:
			if r.State == "available" {
				m, _ := VolumeMonthly(r.Size)
				add(r, "Attached to nothing", m, nil, ActionVolumeDelete)
			}
		case KindAddress:
			if r.State == "unattached" {
				add(r, "Attached to nothing", AddressMonthly, nil, ActionAddressRelease)
			}
		case KindInstance:
			if r.State != "stopped" {
				continue
			}
			at, err := time.Parse(time.RFC3339, r.Details["stoppedAt"])
			if err != nil || now.Sub(at) < IdleStoppedFor {
				continue
			}
			var m float64
			for _, l := range r.Links {
				if v, ok := vols[l.ID]; ok && l.Kind == KindVolume {
					p, _ := VolumeMonthly(v.Size)
					m += p
				}
			}
			add(r, "Stopped for "+daysWords(now.Sub(at))+"; its disks still bill", m, &at, "")
		case KindSnapshot:
			if r.CreatedAt == nil || now.Sub(*r.CreatedAt) < IdleSnapshotAge || usedSnap[r.ID] {
				continue
			}
			gone := true
			for _, l := range r.Links {
				if l.Kind == KindVolume && present[l.ID] {
					gone = false
				}
			}
			if !gone {
				continue
			}
			add(r, daysWords(now.Sub(*r.CreatedAt))+" old, of a disk that is gone, and no image uses it",
				float64(GiB(r.Size))*SnapshotPerGB, r.CreatedAt, ActionSnapshotDelete)
			out[len(out)-1].AtMost = true
		case KindLoadBalancer:
			if r.State != "active" || lbTargets[r.ID] > 0 {
				continue
			}
			why := "No target groups"
			if lbGroups[r.ID] > 0 {
				why = "Its target groups have no targets"
			}
			add(r, why, LoadBalancerMonthly, nil, "")
		}
	}
	slices.SortStableFunc(out, func(a, b IdleFinding) int {
		switch {
		case a.Monthly > b.Monthly:
			return -1
		case a.Monthly < b.Monthly:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func daysWords(d time.Duration) string {
	n := int(d.Hours() / 24)
	if n == 1 {
		return "1 day"
	}
	return strconv.Itoa(n) + " days"
}
