package cloud

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

/*
Reservations: what an account has paid for ahead, and how much of it is in
use. Three things AWS calls by that name:

	Reserved Instances    a discount on instance hours of one type, for one or
	                      three years
	Savings Plans         a discount on a committed spend an hour, for one or
	                      three years
	Capacity Reservations room for instances of one type in one zone, billed
	                      whether or not anything runs in it

Use is worked out from the inventory: which running instances a Reserved
Instance's hours would land on, the way AWS applies them (zonal ones first,
then regional ones, which bend across sizes of a family for Linux). AWS
does the same sum by the second over a month; Argus does it for now, so it
says "estimate" and never claims a number AWS bills. A Capacity
Reservation's use is AWS's own count.
*/

// Reservation kinds, read once per account (stored under GlobalRegion, each
// with its own region).
const (
	KindReservedInstance    Kind = "reserved-instance"
	KindSavingsPlan         Kind = "savings-plan"
	KindCapacityReservation Kind = "capacity-reservation"
	// KindReservationsRead records which parts of the last reservations
	// read worked, so "none" is not confused with "could not look".
	KindReservationsRead Kind = "reservations-read"
)

// ReservationKinds are the kinds the reservations read stores.
var ReservationKinds = []Kind{KindReservedInstance, KindSavingsPlan, KindCapacityReservation, KindReservationsRead}

// sizeUnits are AWS's normalization factors: how many "small" instances a
// size is worth. A regional Linux Reserved Instance covers any size of its
// family by these.
var sizeUnits = map[string]float64{"nano": 0.25, "micro": 0.5, "small": 1, "medium": 2, "large": 4, "xlarge": 8}

// SizeUnits is a size's normalization factor; false for one without a
// factor (metal, or not an instance type).
func SizeUnits(size string) (float64, bool) {
	_, s, ok := strings.Cut(size, ".")
	if !ok {
		return 0, false
	}
	if u, ok := sizeUnits[s]; ok {
		return u, true
	}
	if n, ok := strings.CutSuffix(s, "xlarge"); ok {
		if x, err := strconv.Atoi(n); err == nil && x > 0 {
			return float64(x) * 8, true
		}
	}
	return 0, false
}

// Family is an instance type's family: m6i for m6i.large.
func Family(size string) string {
	f, _, _ := strings.Cut(size, ".")
	return f
}

// RIUse is one Reserved Instance and how much of it running instances use.
type RIUse struct {
	ID      string  `json:"id"`
	Account string  `json:"accountId"`
	Units   float64 `json:"units"`
	Used    float64 `json:"used"`
	// Use is Used / Units, 0 to 1.
	Use float64 `json:"use"`
	// Monthly is what it costs a month, upfront spread over its term.
	Monthly float64 `json:"monthly"`
	// Flexible is a regional Linux one, which covers any size of its family.
	Flexible bool `json:"flexible"`
	// Covers names the instances it lands on.
	Covers []string `json:"covers,omitempty"`
}

// SPUse is one Savings Plan and the on-demand use it could cover now.
type SPUse struct {
	ID      string `json:"id"`
	Account string `json:"accountId"`
	// Commitment is what it bills an hour, used or not.
	Commitment float64 `json:"commitment"`
	// Eligible is the list price an hour of running instances it could
	// apply to that no Reserved Instance already covers.
	Eligible float64 `json:"eligible"`
	// AlsoOther is a Compute plan, which also covers Fargate and Lambda,
	// which Argus does not read.
	AlsoOther bool `json:"alsoOther"`
}

// Coverage is how much of the running instances' list price in one account
// and region a Reserved Instance covers.
type Coverage struct {
	Account   string  `json:"accountId"`
	Region    string  `json:"region"`
	Instances int     `json:"instances"`
	Priced    int     `json:"priced"`
	Hourly    float64 `json:"hourly"`
	ByRI      float64 `json:"byRi"`
	// Rest is the list price an hour left for a Savings Plan or on demand.
	Rest float64 `json:"rest"`
}

// ReservationReport is the use of every reservation in a set of resources.
type ReservationReport struct {
	RIs      []RIUse    `json:"reservedInstances"`
	Plans    []SPUse    `json:"savingsPlans"`
	Coverage []Coverage `json:"coverage"`
}

type runningInstance struct {
	r       Resource
	account string
	units   float64
	left    float64
	exact   bool // no normalization factor: only an exact-type RI fits
	price   float64
	priced  bool
	covered []string
}

// AccountResource is a resource and the account it is in.
type AccountResource struct {
	Account string
	Resource
}

// Active reports whether a reservation is in force.
func reservationActive(r Resource) bool {
	return r.State == "active"
}

// RIHourly is what one instance of a Reserved Instance costs an hour,
// upfront spread over its term.
func RIHourly(r Resource) float64 {
	hourly, _ := strconv.ParseFloat(r.Details["hourly"], 64)
	upfront, _ := strconv.ParseFloat(r.Details["upfront"], 64)
	months, _ := strconv.Atoi(r.Details["termMonths"])
	if months > 0 && upfront > 0 {
		hourly += upfront / (float64(months) / 12 * 8760)
	}
	return hourly
}

func riPlatform(r Resource) string {
	p := strings.TrimSuffix(r.Details["platform"], " (Amazon VPC)")
	if p == "" {
		return "Linux/UNIX"
	}
	return p
}

func instancePlatform(r Resource) string {
	p := r.Details["platform"]
	if p == "" {
		return "Linux/UNIX"
	}
	return p
}

// AnalyseReservations matches Reserved Instances to running instances the
// way AWS applies them, and works out what each Savings Plan could cover.
func AnalyseReservations(rs []AccountResource) ReservationReport {
	var running []*runningInstance
	var ris, plans []AccountResource
	for _, x := range rs {
		switch x.Kind {
		case KindInstance:
			if x.State != "running" {
				continue
			}
			u, ok := SizeUnits(x.Size)
			ri := &runningInstance{r: x.Resource, account: x.Account, units: u, left: u, exact: !ok}
			if !ok {
				ri.units, ri.left = 1, 1
			}
			ri.price, ri.priced = HourlyPrice(x.Size)
			running = append(running, ri)
		case KindReservedInstance:
			if reservationActive(x.Resource) {
				ris = append(ris, x)
			}
		case KindSavingsPlan:
			if reservationActive(x.Resource) {
				plans = append(plans, x)
			}
		}
	}
	slices.SortFunc(running, func(a, b *runningInstance) int { return strings.Compare(a.r.ID, b.r.ID) })
	// Zonal first, as AWS applies them; then the rest by id, so the answer
	// does not move between reads.
	slices.SortStableFunc(ris, func(a, b AccountResource) int {
		za, zb := a.Details["scope"] == "zone", b.Details["scope"] == "zone"
		if za != zb {
			if za {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	out := ReservationReport{RIs: []RIUse{}, Plans: []SPUse{}, Coverage: []Coverage{}}
	for _, x := range ris {
		count, _ := strconv.Atoi(x.Details["count"])
		zonal := x.Details["scope"] == "zone"
		flexible := !zonal && riPlatform(x.Resource) == "Linux/UNIX" && (x.Details["tenancy"] == "" || x.Details["tenancy"] == "default")
		perUnit, okUnits := SizeUnits(x.Size)
		if !flexible || !okUnits {
			flexible, perUnit = false, 1
		}
		u := RIUse{ID: x.ID, Account: x.Account, Units: float64(count) * perUnit, Flexible: flexible,
			Monthly: float64(count) * RIHourly(x.Resource) * HoursPerMonth}
		left := u.Units
		for _, in := range running {
			if left <= 0 {
				break
			}
			if in.account != x.Account || in.left <= 0 || in.r.Region != x.Region || instancePlatform(in.r) != riPlatform(x.Resource) {
				continue
			}
			if zonal && in.r.Zone != x.Zone {
				continue
			}
			var take float64
			if flexible && !in.exact {
				if Family(in.r.Size) != Family(x.Size) {
					continue
				}
				take = math.Min(left, in.left)
			} else {
				// One instance of the type per reserved instance, whole.
				if in.r.Size != x.Size || in.left < in.units {
					continue
				}
				in.left = 0
				left--
				u.Used++
				in.covered = append(in.covered, x.ID)
				u.Covers = append(u.Covers, in.r.Name)
				continue
			}
			in.left -= take
			left -= take
			u.Used += take
			in.covered = append(in.covered, x.ID)
			u.Covers = append(u.Covers, in.r.Name)
		}
		if u.Units > 0 {
			u.Use = math.Min(1, u.Used/u.Units)
		}
		u.Monthly = round2(u.Monthly)
		out.RIs = append(out.RIs, u)
	}
	type key struct{ account, region string }
	cov := map[key]*Coverage{}
	var keys []key
	for _, in := range running {
		k := key{in.account, in.r.Region}
		c := cov[k]
		if c == nil {
			c = &Coverage{Account: in.account, Region: in.r.Region}
			cov[k] = c
			keys = append(keys, k)
		}
		c.Instances++
		if !in.priced {
			continue
		}
		c.Priced++
		c.Hourly += in.price
		share := 1 - in.left/in.units
		c.ByRI += in.price * share
	}
	slices.SortFunc(keys, func(a, b key) int {
		if d := strings.Compare(a.account, b.account); d != 0 {
			return d
		}
		return strings.Compare(a.region, b.region)
	})
	for _, k := range keys {
		c := cov[k]
		c.Rest = round2(c.Hourly - c.ByRI)
		c.Hourly, c.ByRI = round2(c.Hourly), round2(c.ByRI)
		out.Coverage = append(out.Coverage, *c)
	}
	for _, x := range plans {
		commit, _ := strconv.ParseFloat(x.Details["commitment"], 64)
		u := SPUse{ID: x.ID, Account: x.Account, Commitment: commit, AlsoOther: x.Details["type"] == "Compute"}
		for _, in := range running {
			if in.account != x.Account || !in.priced || in.left <= 0 {
				continue
			}
			if x.Details["type"] == "EC2Instance" && (in.r.Region != x.Details["planRegion"] || Family(in.r.Size) != x.Details["family"]) {
				continue
			}
			if x.Details["type"] != "EC2Instance" && x.Details["type"] != "Compute" {
				continue
			}
			u.Eligible += in.price * in.left / in.units
		}
		u.Eligible = round2(u.Eligible)
		out.Plans = append(out.Plans, u)
	}
	return out
}

// reservationFindings are the "paid for, not used" findings: a Reserved
// Instance under 80% used, a Capacity Reservation with room unused, and an
// EC2 Instance Savings Plan with nothing in its family and region to cover.
// Nothing is said in a reservation's first day, while instances move in.
func (f *finder) reservations(rs []Resource) {
	var all []AccountResource
	for _, r := range rs {
		all = append(all, AccountResource{Resource: r})
	}
	rep := AnalyseReservations(all)
	ri := map[string]RIUse{}
	for _, u := range rep.RIs {
		ri[u.ID] = u
	}
	sp := map[string]SPUse{}
	for _, u := range rep.Plans {
		sp[u.ID] = u
	}
	settled := func(r Resource) bool {
		t, err := time.Parse(time.RFC3339, r.Details["start"])
		return err == nil && f.now.Sub(t) >= 24*time.Hour
	}
	for _, r := range rs {
		if !reservationActive(r) || !settled(r) {
			continue
		}
		switch r.Kind {
		case KindReservedInstance:
			u, ok := ri[r.ID]
			if !ok || u.Use >= 0.8 {
				continue
			}
			idle := u.Units - u.Used
			what := r.Details["count"] + " × " + r.Size
			unused := fmt.Sprintf("about %d%% of it is unused", int(math.Round((1-u.Use)*100)))
			if u.Used == 0 {
				unused = "no running instance matches it"
			}
			f.add(Finding{Check: "reservation-unused", Group: GroupWaste, Severity: SeverityMedium, Source: SourceJudgement,
				Title:   "Reserved Instance not used",
				Why:     fmt.Sprintf("Pays for %s in %s; %s, matched against the instances running at the last read.", what, scopeWords(r), unused),
				Matters: "It is billed every hour for its whole term, used or not.",
				HowToFix: "Run matching instances in it, or move instances onto its type. A convertible one can be exchanged in AWS; a standard one " +
					"can be sold on the Reserved Instance Marketplace.",
				Monthly: u.Monthly * idle / u.Units, Since: startOf(r)}, r, fmt.Sprintf("%.2f", u.Used))
		case KindCapacityReservation:
			free, _ := strconv.Atoi(r.Details["available"])
			if free <= 0 {
				continue
			}
			total, _ := strconv.Atoi(r.Details["total"])
			p, _ := HourlyPrice(r.Size)
			f.add(Finding{Check: "capacity-unused", Group: GroupWaste, Severity: SeverityMedium, Source: SourceCloud,
				Title:    "Capacity reserved, not used",
				Why:      fmt.Sprintf("Keeps room for %d %s in %s; %d of it %s empty.", total, r.Size, r.Zone, free, isAre(free)),
				Matters:  "Empty room in a capacity reservation is billed the same as a running instance.",
				HowToFix: "Launch into it, lower its count, or cancel it.",
				Monthly:  float64(free) * p * HoursPerMonth, Since: startOf(r)}, r, strconv.Itoa(free))
		case KindSavingsPlan:
			u, ok := sp[r.ID]
			if !ok || r.Details["type"] != "EC2Instance" || u.Eligible > 0 {
				continue
			}
			f.add(Finding{Check: "savings-plan-idle", Group: GroupWaste, Severity: SeverityMedium, Source: SourceCloud,
				Title:    "Savings Plan with nothing to cover",
				Why:      fmt.Sprintf("Commits $%s an hour to %s instances in %s, and none is running there.", r.Details["commitment"], r.Details["family"], r.Details["planRegion"]),
				Matters:  "The commitment is billed every hour of its term whether anything uses it or not.",
				HowToFix: "Run the work it was bought for on " + r.Details["family"] + " instances in " + r.Details["planRegion"] + ".",
				Monthly:  u.Commitment * HoursPerMonth, Since: startOf(r)}, r, "none")
		}
	}
}

func scopeWords(r Resource) string {
	if r.Details["scope"] == "zone" && r.Zone != "" {
		return r.Zone
	}
	return r.Region
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func startOf(r Resource) *time.Time {
	t, err := time.Parse(time.RFC3339, r.Details["start"])
	if err != nil {
		return nil
	}
	return &t
}
