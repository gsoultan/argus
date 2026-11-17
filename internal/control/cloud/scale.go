package cloud

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

/*
Scale groups: servers that do the same job behind one load balancer, with a
rule for when to add one and when to take one away.

The rule reads Argus's own metrics, the 5-minute buckets every agent sends,
so it can use memory, swap and load that AWS cannot see without its paid
agent. A rule is "signal is above or below value for minutes", over the
group's average or any one member. Adding is "any of" its rules, taking
away "all of" them. A member whose agent has not reported lately is not
counted; when more than half are quiet the group does nothing and says why.

What runs: Argus launches a server from the group's template, puts it in the
target group once it runs, and takes the newest one away by draining it
first. Whether it asks each time or acts within the limits is the group's
mode, and the safety limits below hold whatever the mode.
*/

// Scale group actions.
const (
	ActionScaleGroupCreate Action = "scale-group-create"
	ActionScaleGroupChange Action = "scale-group-change"
	ActionScaleAdd         Action = "scale-add"
	ActionScaleRemove      Action = "scale-remove"
)

// IsScale is whether a is one of the scale group actions.
func IsScale(a Action) bool {
	return a == ActionScaleGroupCreate || a == ActionScaleGroupChange || a == ActionScaleAdd || a == ActionScaleRemove
}

// ScaleActor is who Argus's own scale changes run as, so CloudTrail shows
// them apart from a person's.
const ScaleActor Actor = "argus-scale"

// ReplicaActor is who Argus runs as when it keeps a database's read
// endpoint: a replica in or out of its target group.
const ReplicaActor Actor = "argus-replicas"

// Safety limits no setting removes.
const (
	// ScaleAskLapse is how long an "Ask each time" question waits: a
	// scale-out approved an hour late is usually wrong.
	ScaleAskLapse = 30 * time.Minute
	// ScaleAutoAddsPerDay is the most adds Argus makes by itself in a day;
	// the next is asked.
	ScaleAutoAddsPerDay = 3
	// ScaleFailedStarts is how many new servers in a row may fail their
	// health check before the group stops adding.
	ScaleFailedStarts = 2
	// ScaleHealthWait is how long a new server has to become healthy.
	ScaleHealthWait = 15 * time.Minute
	// ScaleDrain is how long a server taken away is drained before it is
	// terminated: the target group's default deregistration delay.
	ScaleDrain = 300 * time.Second
	// ScaleQuiet is how long since a member's last report before it is
	// not counted.
	ScaleQuiet = 10 * time.Minute
	// MaxScaleGroup is the largest maximum a group can have.
	MaxScaleGroup = 20
)

// Signals a rule can watch.
const (
	SignalCPU    = "cpu"    // processor busy, %
	SignalMemory = "memory" // memory available, %
	SignalSwap   = "swap"   // swap in use, %
	SignalLoad   = "load"   // load per core
	SignalSteal  = "steal"  // processor steal, %
)

var signalWords = map[string]string{
	SignalCPU: "processor", SignalMemory: "memory available", SignalSwap: "swap in use",
	SignalLoad: "load per core", SignalSteal: "processor steal",
}

// ScaleRule is "signal is above or below value for minutes", over the
// group's average or any member.
type ScaleRule struct {
	Signal  string  `json:"signal"`
	Over    string  `json:"over"` // "average" or "any"
	Op      string  `json:"op"`   // "above" or "below"
	Value   float64 `json:"value"`
	Minutes int     `json:"minutes"`
}

// Check says what is wrong with r, or nil.
func (r ScaleRule) Check() error {
	if _, ok := signalWords[r.Signal]; !ok {
		return fmt.Errorf("%q is not a signal Argus watches; use processor, memory, swap, load or steal", r.Signal)
	}
	if r.Over != "average" && r.Over != "any" {
		return errors.New("a rule is over the group's average or any member")
	}
	if r.Op != "above" && r.Op != "below" {
		return errors.New("a rule is above or below a value")
	}
	hi := 100.0
	if r.Signal == SignalLoad {
		hi = 16
	}
	if r.Value <= 0 || r.Value >= hi || math.IsNaN(r.Value) {
		return fmt.Errorf("the value for %s is between 0 and %g", signalWords[r.Signal], hi)
	}
	if r.Minutes < 5 || r.Minutes > 120 || r.Minutes%5 != 0 {
		return errors.New("a rule holds for 5 to 120 minutes, in fives: Argus's metrics are 5-minute buckets")
	}
	return nil
}

// Words says r: "group average processor above 75% for 10 min".
func (r ScaleRule) Words() string {
	who := "group average"
	if r.Over == "any" {
		who = "any member's"
	}
	v := strconv.FormatFloat(r.Value, 'f', -1, 64)
	if r.Signal != SignalLoad {
		v += "%"
	}
	return fmt.Sprintf("%s %s %s %s for %d min", who, signalWords[r.Signal], r.Op, v, r.Minutes)
}

// ScaleRules are when to add a server (any of) and when to take one away
// (all of).
type ScaleRules struct {
	Add    []ScaleRule `json:"add"`
	Remove []ScaleRule `json:"remove"`
}

// DefaultScaleRules are the wizard's: add when the group's processor is
// above 75% or any member's memory available below 15% for 10 minutes;
// take away when processor is below 25% for 30 minutes and memory is fine.
func DefaultScaleRules() ScaleRules {
	return ScaleRules{
		Add: []ScaleRule{
			{Signal: SignalCPU, Over: "average", Op: "above", Value: 75, Minutes: 10},
			{Signal: SignalMemory, Over: "any", Op: "below", Value: 15, Minutes: 10},
		},
		Remove: []ScaleRule{
			{Signal: SignalCPU, Over: "average", Op: "below", Value: 25, Minutes: 30},
			{Signal: SignalMemory, Over: "average", Op: "above", Value: 30, Minutes: 30},
		},
	}
}

// Check says what is wrong with the rules, or nil.
func (rs ScaleRules) Check() error {
	if len(rs.Add) == 0 || len(rs.Add) > 4 {
		return errors.New("give 1 to 4 rules for adding a server")
	}
	if len(rs.Remove) > 4 {
		return errors.New("give at most 4 rules for taking one away")
	}
	for _, r := range append(slices.Clone(rs.Add), rs.Remove...) {
		if err := r.Check(); err != nil {
			return err
		}
	}
	return nil
}

// ScaleLimits are the group's size, cost cap and waits.
type ScaleLimits struct {
	Min int `json:"min"`
	Max int `json:"max"`
	// CostCap is the most the group's servers may cost a month, in dollars;
	// 0 is none.
	CostCap float64 `json:"costCap"`
	// AddWait and RemoveWait are the minutes after adding or taking away
	// before the next.
	AddWait    int `json:"addWait"`
	RemoveWait int `json:"removeWait"`
}

// Check says what is wrong with l, or nil.
func (l ScaleLimits) Check() error {
	if l.Min < 1 || l.Max < l.Min || l.Max > MaxScaleGroup {
		return fmt.Errorf("a group keeps at least 1 server, and its maximum is from its minimum up to %d", MaxScaleGroup)
	}
	if l.CostCap < 0 || l.CostCap > 1_000_000 {
		return errors.New("the cost cap is a monthly amount in dollars, or 0 for none")
	}
	if l.AddWait < 5 || l.AddWait > 240 || l.RemoveWait < 5 || l.RemoveWait > 240 {
		return errors.New("wait 5 to 240 minutes after adding or taking away")
	}
	return nil
}

// ScaleModes are who acts when a rule is met.
const (
	ScaleAsk  = "ask"  // each add or take-away goes to Approvals
	ScaleAuto = "auto" // Argus acts within the limits
)

// ScaleSample is one member's 5-minute bucket.
type ScaleSample struct {
	At     time.Time
	Values map[string]float64 // by signal; a missing signal was not reported
}

// ScaleVerdict is what the rules say now.
type ScaleVerdict struct {
	Add    bool `json:"add"`
	Remove bool `json:"remove"`
	// Why is the rule met, with what each member was: "group average
	// processor above 75% for 10 min: web-01 88%, web-02 84%".
	Why string `json:"why"`
	// Held is why the group does nothing, when it cannot tell.
	Held string `json:"held,omitempty"`
	// Counted are the members whose metrics were read; Quiet those not.
	Counted []string `json:"counted"`
	Quiet   []string `json:"quiet"`
}

// EvaluateScale reads the rules against each member's samples at now.
func EvaluateScale(rs ScaleRules, samples map[string][]ScaleSample, members []string, now time.Time) ScaleVerdict {
	v := ScaleVerdict{Counted: []string{}, Quiet: []string{}}
	live := map[string][]ScaleSample{}
	for _, m := range members {
		ss := samples[m]
		if len(ss) == 0 || now.Sub(ss[len(ss)-1].At) > ScaleQuiet {
			v.Quiet = append(v.Quiet, m)
			continue
		}
		live[m] = ss
		v.Counted = append(v.Counted, m)
	}
	if len(members) == 0 {
		v.Held = "it has no servers yet"
		return v
	}
	if len(v.Quiet)*2 > len(members) {
		v.Held = fmt.Sprintf("%d of %d servers have not reported in %d minutes (%s), so Argus cannot tell how busy the group is",
			len(v.Quiet), len(members), int(ScaleQuiet.Minutes()), strings.Join(v.Quiet, ", "))
		return v
	}
	for _, r := range rs.Add {
		if ok, why := ruleMet(r, live, now); ok {
			v.Add, v.Why = true, why
			return v
		}
	}
	if len(rs.Remove) == 0 {
		return v
	}
	var whys []string
	for _, r := range rs.Remove {
		ok, why := ruleMet(r, live, now)
		if !ok {
			return v
		}
		whys = append(whys, why)
	}
	v.Remove, v.Why = true, strings.Join(whys, "; and ")
	return v
}

// ruleMet is whether r held over its whole window, and in words, what
// each member was on average over it.
func ruleMet(r ScaleRule, live map[string][]ScaleSample, now time.Time) (bool, string) {
	from := now.Add(-time.Duration(r.Minutes) * time.Minute)
	// A window of m minutes is m/5 buckets; allow one missing, at the edge.
	need := max(1, r.Minutes/5-1)
	names := make([]string, 0, len(live))
	for m := range live {
		names = append(names, m)
	}
	sort.Strings(names)
	holds := func(x float64) bool {
		if r.Op == "above" {
			return x > r.Value
		}
		return x < r.Value
	}
	means := map[string]float64{}
	perBucket := map[time.Time][]float64{}
	memberHolds := map[string]bool{}
	for _, m := range names {
		n, sum, ok := 0, 0.0, true
		for _, s := range live[m] {
			if s.At.Before(from) || s.At.After(now) {
				continue
			}
			x, has := s.Values[r.Signal]
			if !has {
				continue
			}
			n++
			sum += x
			perBucket[s.At] = append(perBucket[s.At], x)
			ok = ok && holds(x)
		}
		if n > 0 {
			means[m] = sum / float64(n)
		}
		memberHolds[m] = ok && n >= need
	}
	each := func(only func(string) bool) string {
		var parts []string
		for _, m := range names {
			if x, ok := means[m]; ok && only(m) {
				parts = append(parts, m+" "+fmtSignal(r.Signal, x))
			}
		}
		return strings.Join(parts, ", ")
	}
	if r.Over == "any" {
		for _, m := range names {
			if memberHolds[m] {
				return true, r.Words() + ": " + each(func(n string) bool { return n == m })
			}
		}
		return false, ""
	}
	if len(perBucket) < need {
		return false, ""
	}
	sum := 0.0
	for _, xs := range perBucket {
		avg := 0.0
		for _, x := range xs {
			avg += x
		}
		avg /= float64(len(xs))
		if !holds(avg) {
			return false, ""
		}
		sum += avg
	}
	return true, fmt.Sprintf("%s (%s): %s", r.Words(), fmtSignal(r.Signal, sum/float64(len(perBucket))), each(func(string) bool { return true }))
}

func fmtSignal(signal string, x float64) string {
	if signal == SignalLoad {
		return strconv.FormatFloat(math.Round(x*100)/100, 'f', -1, 64)
	}
	return fmt.Sprintf("%.0f%%", x)
}
