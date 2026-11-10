package agent

import (
	"testing"
	"time"
)

func TestNetRates(t *testing.T) {
	var u UsageSampler
	t0 := time.Unix(1_700_000_000, 0)
	first := u.netRates([]netCounters{{name: "eth0", rx: 1000, tx: 500, speedMbps: 1000}}, t0)
	if len(first) != 1 || first[0].RxBps != nil || first[0].TxBps != nil {
		t.Fatalf("first sample = %+v, want no rate with nothing to compare", first)
	}
	if first[0].SpeedMbps != 1000 {
		t.Errorf("speed = %d", first[0].SpeedMbps)
	}

	// 60 s later: 6000 more bytes in (100 B/s), and the out counter went
	// backwards (interface reset), which has no honest rate.
	second := u.netRates([]netCounters{{name: "eth0", rx: 7000, tx: 100}, {name: "eth1", rx: 5, tx: 5}}, t0.Add(time.Minute))
	if len(second) != 2 {
		t.Fatalf("second = %+v", second)
	}
	if second[0].RxBps == nil || *second[0].RxBps != 100 {
		t.Errorf("rx = %v, want 100", second[0].RxBps)
	}
	if second[0].TxBps != nil {
		t.Errorf("tx = %v after a counter reset, want none", *second[0].TxBps)
	}
	if second[1].RxBps != nil {
		t.Errorf("a new interface has a rate on its first sighting: %v", *second[1].RxBps)
	}

	// Bounded however many interfaces the platform lists.
	many := make([]netCounters, maxNet+10)
	for i := range many {
		many[i] = netCounters{name: string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	if got := u.netRates(many, t0.Add(2*time.Minute)); len(got) != maxNet {
		t.Errorf("rates = %d, want bounded at %d", len(got), maxNet)
	}
}
