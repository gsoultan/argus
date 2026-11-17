package cloud

import (
	"strings"
	"testing"
	"time"
)

func samplesOf(now time.Time, minutes int, vals map[string]float64) []ScaleSample {
	var out []ScaleSample
	for m := minutes; m >= 0; m -= 5 {
		v := map[string]float64{}
		for k, x := range vals {
			v[k] = x
		}
		out = append(out, ScaleSample{At: now.Add(-time.Duration(m) * time.Minute), Values: v})
	}
	return out
}

func TestScaleRuleChecks(t *testing.T) {
	if err := DefaultScaleRules().Check(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ScaleRule{
		{Signal: "disk", Over: "average", Op: "above", Value: 50, Minutes: 10},
		{Signal: SignalCPU, Over: "most", Op: "above", Value: 50, Minutes: 10},
		{Signal: SignalCPU, Over: "average", Op: "near", Value: 50, Minutes: 10},
		{Signal: SignalCPU, Over: "average", Op: "above", Value: 100, Minutes: 10},
		{Signal: SignalCPU, Over: "average", Op: "above", Value: 50, Minutes: 7},
		{Signal: SignalLoad, Over: "average", Op: "above", Value: 20, Minutes: 10},
	} {
		if bad.Check() == nil {
			t.Fatalf("%+v passed", bad)
		}
	}
	if (ScaleRules{}).Check() == nil {
		t.Fatal("no add rule passed")
	}
	for _, bad := range []ScaleLimits{{Min: 0, Max: 2, AddWait: 10, RemoveWait: 30}, {Min: 3, Max: 2, AddWait: 10, RemoveWait: 30},
		{Min: 1, Max: 21, AddWait: 10, RemoveWait: 30}, {Min: 1, Max: 2, AddWait: 1, RemoveWait: 30}} {
		if bad.Check() == nil {
			t.Fatalf("%+v passed", bad)
		}
	}
	if w := DefaultScaleRules().Add[0].Words(); w != "group average processor above 75% for 10 min" {
		t.Fatal(w)
	}
}

func TestEvaluateScale(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	rs := DefaultScaleRules()
	members := []string{"web-01", "web-02"}
	busy := map[string][]ScaleSample{
		"web-01": samplesOf(now, 40, map[string]float64{SignalCPU: 88, SignalMemory: 50}),
		"web-02": samplesOf(now, 40, map[string]float64{SignalCPU: 84, SignalMemory: 50}),
	}
	v := EvaluateScale(rs, busy, members, now)
	if !v.Add || v.Remove || !strings.Contains(v.Why, "group average processor above 75% for 10 min (86%): web-01 88%, web-02 84%") {
		t.Fatalf("busy: %+v", v)
	}
	// One member short of memory is enough on its own.
	tight := map[string][]ScaleSample{
		"web-01": samplesOf(now, 40, map[string]float64{SignalCPU: 30, SignalMemory: 10}),
		"web-02": samplesOf(now, 40, map[string]float64{SignalCPU: 30, SignalMemory: 60}),
	}
	if v := EvaluateScale(rs, tight, members, now); !v.Add || !strings.HasPrefix(v.Why, "any member's memory available below 15% for 10 min: web-01 10%") {
		t.Fatalf("tight: %+v", v)
	}
	// Busy for only the last 5 minutes is not 10.
	spike := map[string][]ScaleSample{
		"web-01": append(samplesOf(now.Add(-10*time.Minute), 30, map[string]float64{SignalCPU: 40, SignalMemory: 50}),
			samplesOf(now, 5, map[string]float64{SignalCPU: 95, SignalMemory: 50})...),
		"web-02": samplesOf(now, 40, map[string]float64{SignalCPU: 40, SignalMemory: 50}),
	}
	if v := EvaluateScale(rs, spike, members, now); v.Add || v.Remove {
		t.Fatalf("spike: %+v", v)
	}
	// Quiet takes away only when every take-away rule holds.
	quiet := map[string][]ScaleSample{
		"web-01": samplesOf(now, 40, map[string]float64{SignalCPU: 10, SignalMemory: 70}),
		"web-02": samplesOf(now, 40, map[string]float64{SignalCPU: 12, SignalMemory: 70}),
	}
	if v := EvaluateScale(rs, quiet, members, now); !v.Remove || !strings.Contains(v.Why, "; and group average memory available above 30%") {
		t.Fatalf("quiet: %+v", v)
	}
	quiet["web-02"] = samplesOf(now, 40, map[string]float64{SignalCPU: 12, SignalMemory: 20})
	quiet["web-01"] = samplesOf(now, 40, map[string]float64{SignalCPU: 10, SignalMemory: 30})
	if v := EvaluateScale(rs, quiet, members, now); v.Remove {
		t.Fatalf("memory not fine: %+v", v)
	}
	// A member gone quiet is not counted; more than half quiet holds.
	stale := map[string][]ScaleSample{"web-01": samplesOf(now.Add(-30*time.Minute), 30, map[string]float64{SignalCPU: 99})}
	if v := EvaluateScale(rs, stale, members, now); v.Add || !strings.Contains(v.Held, "2 of 2 servers have not reported") {
		t.Fatalf("stale: %+v", v)
	}
	three := []string{"web-01", "web-02", "web-03"}
	if v := EvaluateScale(rs, busy, three, now); !v.Add || len(v.Quiet) != 1 || v.Quiet[0] != "web-03" {
		t.Fatalf("one quiet: %+v", v)
	}
}
