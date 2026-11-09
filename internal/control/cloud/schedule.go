package cloud

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

/*
Schedule maths: when instances on a schedule should be running, and the
starts and stops between two times. Pure: no clock, no database, no cloud.

A window opens on each of its days at Start and closes at Stop, in the
schedule's zone. Stop is on the same day, or the next day when it is not
after Start (a night shift, 22:00 to 06:00), or on StopDay when that is
set (Monday 07:00 to Friday 21:00). Windows that overlap or touch are one
stretch of running, so an instance is never stopped and started again in
the same minute.

Daylight saving: a time the clock skips (02:30 on the night clocks go
forward) happens at the first minute after the gap; a time the clock
repeats (01:30 on the night they go back) happens once, the first time.
A skip date is a day, in the zone, on which no window opens; a window that
opened the day before still closes as usual.
*/

// Window is one weekly window. Days are ISO weekdays: Monday is 1, Sunday
// is 7.
type Window struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	Stop  string `json:"stop"`
	// StopDay is the weekday the window closes on, when that is not the
	// day it opens or the next.
	StopDay int `json:"stopDay,omitempty"`
}

// Plan is a schedule's timing.
type Plan struct {
	Zone    *time.Location
	Windows []Window
	// Skip holds days as YYYY-MM-DD in Zone.
	Skip []string
}

// Transition is one start or stop.
type Transition struct {
	At     time.Time `json:"at"`
	Action Action    `json:"action"`
}

// Stretch is one time the instances run, from Start to Stop.
type Stretch struct {
	Start time.Time
	Stop  time.Time
}

var reClock = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ParseClock reads "HH:MM".
func ParseClock(s string) (h, m int, ok bool) {
	g := reClock.FindStringSubmatch(s)
	if g == nil {
		return 0, 0, false
	}
	h, _ = strconv.Atoi(g[1])
	m, _ = strconv.Atoi(g[2])
	return h, m, true
}

var reDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// MaxWindows bounds a schedule.
const MaxWindows = 21

// Validate says what is wrong with a plan, in words a person can act on.
func (p Plan) Validate() error {
	if p.Zone == nil {
		return errors.New("pick a time zone")
	}
	if len(p.Windows) == 0 {
		return errors.New("add at least one window")
	}
	if len(p.Windows) > MaxWindows {
		return fmt.Errorf("at most %d windows", MaxWindows)
	}
	for i, w := range p.Windows {
		n := i + 1
		if len(w.Days) == 0 {
			return fmt.Errorf("window %d has no days", n)
		}
		seen := map[int]bool{}
		for _, d := range w.Days {
			if d < 1 || d > 7 {
				return fmt.Errorf("window %d: days are 1 (Monday) to 7 (Sunday)", n)
			}
			if seen[d] {
				return fmt.Errorf("window %d names a day twice", n)
			}
			seen[d] = true
		}
		sh, sm, ok1 := ParseClock(w.Start)
		eh, em, ok2 := ParseClock(w.Stop)
		if !ok1 || !ok2 {
			return fmt.Errorf("window %d: times are HH:MM, like 08:00", n)
		}
		if w.StopDay != 0 {
			if w.StopDay < 1 || w.StopDay > 7 {
				return fmt.Errorf("window %d: the stop day is 1 (Monday) to 7 (Sunday)", n)
			}
			if len(w.Days) != 1 {
				return fmt.Errorf("window %d: a window that stops on another day starts on one day", n)
			}
		} else if sh*60+sm == eh*60+em {
			return fmt.Errorf("window %d starts and stops at the same time", n)
		}
	}
	if len(p.Skip) > 366 {
		return errors.New("at most 366 skip dates")
	}
	for _, d := range p.Skip {
		if !reDate.MatchString(d) {
			return fmt.Errorf("%q is not a date like 2026-12-25", d)
		}
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return fmt.Errorf("%q is not a date", d)
		}
	}
	return nil
}

// isoDay is t's weekday with Monday as 1 and Sunday as 7.
func isoDay(t time.Time) int {
	d := int(t.Weekday())
	if d == 0 {
		return 7
	}
	return d
}

// localAt is the instant the clock in loc reads h:m on the given day, with
// the daylight saving rule above.
func localAt(y int, mo time.Month, d, h, m int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, m, 0, 0, loc)
	if t.Hour() != h || t.Minute() != m {
		// In a gap. Go has moved the time by the gap's length, before or
		// after it; the first minute after the gap is where the later of
		// the two zones begins.
		start, end := t.ZoneBounds()
		if t.Hour()*60+t.Minute() < h*60+m || t.Day() != d {
			if !end.IsZero() {
				return end
			}
		} else if !start.IsZero() {
			return start
		}
		return t
	}
	// Repeated: take the earliest instant that reads the same.
	for _, back := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour} {
		e := t.Add(-back)
		if e.Hour() == h && e.Minute() == m && e.Day() == d {
			return e
		}
	}
	return t
}

// Stretches lists the times the instances run that overlap [from, to],
// merged and in order.
func (p Plan) Stretches(from, to time.Time) []Stretch {
	if p.Zone == nil || !to.After(from) {
		return nil
	}
	skip := map[string]bool{}
	for _, d := range p.Skip {
		skip[d] = true
	}
	// A window opens at most a week before it closes, so look back eight
	// days for one still open at from.
	lf := from.In(p.Zone).AddDate(0, 0, -8)
	lt := to.In(p.Zone)
	var all []Stretch
	for day := time.Date(lf.Year(), lf.Month(), lf.Day(), 12, 0, 0, 0, p.Zone); !day.After(lt.AddDate(0, 0, 1)); day = day.AddDate(0, 0, 1) {
		if skip[day.Format(time.DateOnly)] {
			continue
		}
		wd := isoDay(day)
		for _, w := range p.Windows {
			if !containsInt(w.Days, wd) {
				continue
			}
			sh, sm, ok1 := ParseClock(w.Start)
			eh, em, ok2 := ParseClock(w.Stop)
			if !ok1 || !ok2 {
				continue
			}
			ahead := 0
			if w.StopDay != 0 {
				ahead = (w.StopDay - wd + 7) % 7
				if ahead == 0 && eh*60+em <= sh*60+sm {
					ahead = 7
				}
			} else if eh*60+em <= sh*60+sm {
				ahead = 1
			}
			end := day.AddDate(0, 0, ahead)
			s := Stretch{Start: localAt(day.Year(), day.Month(), day.Day(), sh, sm, p.Zone),
				Stop: localAt(end.Year(), end.Month(), end.Day(), eh, em, p.Zone)}
			if s.Stop.After(s.Start) {
				all = append(all, s)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Start.Before(all[j].Start) })
	var merged []Stretch
	for _, s := range all {
		if n := len(merged); n > 0 && !s.Start.After(merged[n-1].Stop) {
			if s.Stop.After(merged[n-1].Stop) {
				merged[n-1].Stop = s.Stop
			}
			continue
		}
		merged = append(merged, s)
	}
	out := merged[:0]
	for _, s := range merged {
		if s.Stop.After(from) && !s.Start.After(to) {
			out = append(out, s)
		}
	}
	return out
}

// Transitions lists the starts and stops after from, up to and including
// to, in order.
func (p Plan) Transitions(from, to time.Time) []Transition {
	var out []Transition
	in := func(t time.Time) bool { return t.After(from) && !t.After(to) }
	for _, s := range p.Stretches(from, to) {
		if in(s.Start) {
			out = append(out, Transition{At: s.Start.UTC(), Action: ActionStart})
		}
		if in(s.Stop) {
			out = append(out, Transition{At: s.Stop.UTC(), Action: ActionStop})
		}
	}
	return out
}

// Running reports whether instances on the plan should be running at t.
func (p Plan) Running(t time.Time) bool {
	for _, s := range p.Stretches(t.Add(-time.Minute), t) {
		if !t.Before(s.Start) && t.Before(s.Stop) {
			return true
		}
	}
	return false
}

// HoursPerWeek is how long the instances run in an ordinary week, averaged
// over the four weeks from t, skip dates included.
func (p Plan) HoursPerWeek(t time.Time) float64 {
	end := t.AddDate(0, 0, 28)
	var total time.Duration
	for _, s := range p.Stretches(t, end) {
		a, b := s.Start, s.Stop
		if a.Before(t) {
			a = t
		}
		if b.After(end) {
			b = end
		}
		total += b.Sub(a)
	}
	return total.Hours() / 4
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ScheduleTag is the tag that puts an instance on a schedule.
const ScheduleTag = "ArgusSchedule"

// ScheduleOf is the schedule an instance's tags name, or "".
func ScheduleOf(tags map[string]string) string {
	for k, v := range tags {
		if strings.EqualFold(k, ScheduleTag) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// StopRefusal says why a scheduled stop would not work on an instance, or
// "" when it can be stopped. An instance-store root loses its disk when
// stopped and AWS refuses; a spot instance may be a one-time request,
// which cannot be stopped either.
func StopRefusal(r Resource) string {
	if r.Details["rootDevice"] == "instance-store" {
		return "its root disk is instance store, which AWS cannot stop"
	}
	if r.Details["lifecycle"] == "spot" {
		return "it is a spot instance; schedules leave those alone"
	}
	return ""
}

// hourly is the on-demand Linux price per hour, in US dollars, in
// us-east-1, of the sizes the saving estimate knows. Other regions differ
// by a few percent; the estimate says it is one.
var hourly = map[string]float64{
	"t3.nano": 0.0052, "t3.micro": 0.0104, "t3.small": 0.0208, "t3.medium": 0.0416, "t3.large": 0.0832,
	"t3.xlarge": 0.1664, "t3.2xlarge": 0.3328,
	"t4g.nano": 0.0042, "t4g.micro": 0.0084, "t4g.small": 0.0168, "t4g.medium": 0.0336, "t4g.large": 0.0672,
	"t4g.xlarge": 0.1344, "t4g.2xlarge": 0.2688,
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768,
	"m7i.large": 0.1008, "m7i.xlarge": 0.2016, "m7i.2xlarge": 0.4032, "m7i.4xlarge": 0.8064,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68,
	"c7i.large": 0.0893, "c7i.xlarge": 0.1785, "c7i.2xlarge": 0.357, "c7i.4xlarge": 0.714,
	"r6i.large": 0.126, "r6i.xlarge": 0.252, "r6i.2xlarge": 0.504, "r6i.4xlarge": 1.008,
	"r7i.large": 0.1323, "r7i.xlarge": 0.2646, "r7i.2xlarge": 0.5292, "r7i.4xlarge": 1.0584,
}

// HourlyPrice is a size's on-demand price per hour, when the estimate
// knows it.
func HourlyPrice(size string) (float64, bool) {
	p, ok := hourly[size]
	return p, ok
}
