package cloud

import (
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var officeHours = []Window{{Days: []int{1, 2, 3, 4, 5}, Start: "08:00", Stop: "19:00"}}

func words(ts []Transition, loc *time.Location) []string {
	var out []string
	for _, x := range ts {
		out = append(out, string(x.Action)+" "+x.At.In(loc).Format("Mon 2006-01-02 15:04 MST"))
	}
	return out
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d transitions %q, want %q", len(got), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("transition %d: got %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}

func TestScheduleOfficeHoursInJakarta(t *testing.T) {
	loc := zone(t, "Asia/Jakarta")
	p := Plan{Zone: loc, Windows: officeHours}
	// Friday 2026-11-06 noon to Tuesday noon: stop Friday, start and stop
	// Monday, start Tuesday.
	got := words(p.Transitions(at(t, loc, "2026-11-06 12:00"), at(t, loc, "2026-11-10 12:00")), loc)
	same(t, got, []string{
		"stop Fri 2026-11-06 19:00 WIB",
		"start Mon 2026-11-09 08:00 WIB",
		"stop Mon 2026-11-09 19:00 WIB",
		"start Tue 2026-11-10 08:00 WIB",
	})
	if !p.Running(at(t, loc, "2026-11-09 08:00")) || p.Running(at(t, loc, "2026-11-09 19:00")) ||
		p.Running(at(t, loc, "2026-11-08 12:00")) || !p.Running(at(t, loc, "2026-11-09 18:59")) {
		t.Fatal("running is wrong at the window's edges or on a weekend")
	}
	if h := p.HoursPerWeek(at(t, loc, "2026-11-09 00:00")); h != 55 {
		t.Fatalf("office hours run 55 hours a week, got %v", h)
	}
}

func TestScheduleWeekdaysOnlySpansTheWeek(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	p := Plan{Zone: loc, Windows: []Window{{Days: []int{1}, Start: "07:00", StopDay: 5, Stop: "21:00"}}}
	got := words(p.Transitions(at(t, loc, "2026-11-08 00:00"), at(t, loc, "2026-11-15 00:00")), loc)
	same(t, got, []string{"start Mon 2026-11-09 07:00 CET", "stop Fri 2026-11-13 21:00 CET"})
	if !p.Running(at(t, loc, "2026-11-11 03:00")) {
		t.Fatal("a Monday-to-Friday window must run through Wednesday night")
	}
}

func TestScheduleWindowCrossesMidnight(t *testing.T) {
	loc := zone(t, "Asia/Jakarta")
	p := Plan{Zone: loc, Windows: []Window{{Days: []int{6}, Start: "22:00", Stop: "06:00"}}}
	got := words(p.Transitions(at(t, loc, "2026-11-07 00:00"), at(t, loc, "2026-11-09 00:00")), loc)
	same(t, got, []string{"start Sat 2026-11-07 22:00 WIB", "stop Sun 2026-11-08 06:00 WIB"})
	if !p.Running(at(t, loc, "2026-11-08 02:00")) {
		t.Fatal("still running after midnight")
	}
}

func TestScheduleOverlappingWindowsMerge(t *testing.T) {
	loc := zone(t, "UTC")
	p := Plan{Zone: loc, Windows: []Window{
		{Days: []int{1}, Start: "08:00", Stop: "12:00"},
		{Days: []int{1}, Start: "11:00", Stop: "15:00"},
		{Days: []int{1}, Start: "15:00", Stop: "16:00"},
	}}
	got := words(p.Transitions(at(t, loc, "2026-11-09 00:00"), at(t, loc, "2026-11-10 00:00")), loc)
	same(t, got, []string{"start Mon 2026-11-09 08:00 UTC", "stop Mon 2026-11-09 16:00 UTC"})
}

func TestScheduleSkipDates(t *testing.T) {
	loc := zone(t, "Asia/Jakarta")
	p := Plan{Zone: loc, Windows: officeHours, Skip: []string{"2026-12-25"}}
	got := words(p.Transitions(at(t, loc, "2026-12-24 12:00"), at(t, loc, "2026-12-26 12:00")), loc)
	same(t, got, []string{"stop Thu 2026-12-24 19:00 WIB"})
	// A night window that opened the day before a skip date still closes.
	n := Plan{Zone: loc, Windows: []Window{{Days: []int{4, 5}, Start: "22:00", Stop: "06:00"}}, Skip: []string{"2026-12-25"}}
	got = words(n.Transitions(at(t, loc, "2026-12-24 12:00"), at(t, loc, "2026-12-26 12:00")), loc)
	same(t, got, []string{"start Thu 2026-12-24 22:00 WIB", "stop Fri 2026-12-25 06:00 WIB"})
}

func TestScheduleDaylightSavingNewYork(t *testing.T) {
	loc := zone(t, "America/New_York")
	// Clocks go forward at 02:00 on 2026-03-08: 02:30 does not exist and
	// runs at 03:00.
	p := Plan{Zone: loc, Windows: []Window{{Days: []int{7}, Start: "02:30", Stop: "04:00"}}}
	got := words(p.Transitions(at(t, loc, "2026-03-07 00:00"), at(t, loc, "2026-03-09 00:00")), loc)
	same(t, got, []string{"start Sun 2026-03-08 03:00 EDT", "stop Sun 2026-03-08 04:00 EDT"})
	// Clocks go back at 02:00 on 2026-11-01: 01:30 happens twice and runs
	// once, the first time.
	b := Plan{Zone: loc, Windows: []Window{{Days: []int{7}, Start: "01:30", Stop: "05:00"}}}
	ts := b.Transitions(at(t, loc, "2026-10-31 00:00"), at(t, loc, "2026-11-02 00:00"))
	same(t, words(ts, loc), []string{"start Sun 2026-11-01 01:30 EDT", "stop Sun 2026-11-01 05:00 EST"})
	if ts[0].At.UTC().Format("15:04") != "05:30" {
		t.Fatalf("the repeated 01:30 must be the first one (05:30 UTC), got %s", ts[0].At.UTC())
	}
}

func TestScheduleDaylightSavingBerlinOfficeHours(t *testing.T) {
	loc := zone(t, "Europe/Berlin")
	p := Plan{Zone: loc, Windows: officeHours}
	// The week clocks go back (2026-10-25) keeps 08:00 local both sides.
	got := words(p.Transitions(at(t, loc, "2026-10-23 12:00"), at(t, loc, "2026-10-26 12:00")), loc)
	same(t, got, []string{"stop Fri 2026-10-23 19:00 CEST", "start Mon 2026-10-26 08:00 CET"})
}

func TestScheduleValidate(t *testing.T) {
	loc := zone(t, "UTC")
	bad := []Plan{
		{Windows: officeHours},
		{Zone: loc},
		{Zone: loc, Windows: []Window{{Days: []int{0}, Start: "08:00", Stop: "09:00"}}},
		{Zone: loc, Windows: []Window{{Days: []int{1, 1}, Start: "08:00", Stop: "09:00"}}},
		{Zone: loc, Windows: []Window{{Days: []int{1}, Start: "8:00", Stop: "09:00"}}},
		{Zone: loc, Windows: []Window{{Days: []int{1}, Start: "08:00", Stop: "08:00"}}},
		{Zone: loc, Windows: []Window{{Days: []int{1, 2}, Start: "08:00", StopDay: 5, Stop: "09:00"}}},
		{Zone: loc, Windows: officeHours, Skip: []string{"2026-02-30"}},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("plan %d should be refused", i)
		}
	}
	if err := (Plan{Zone: loc, Windows: officeHours, Skip: []string{"2026-12-25"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStopRefusal(t *testing.T) {
	if StopRefusal(Resource{Details: map[string]string{"rootDevice": "instance-store"}}) == "" ||
		StopRefusal(Resource{Details: map[string]string{"lifecycle": "spot"}}) == "" ||
		StopRefusal(Resource{Details: map[string]string{"rootDevice": "ebs"}}) != "" {
		t.Fatal("stop refusal is wrong")
	}
	if ScheduleOf(map[string]string{"argusschedule": " office "}) != "office" {
		t.Fatal("the tag key is matched without case")
	}
}
